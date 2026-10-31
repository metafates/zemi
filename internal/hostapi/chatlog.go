// chatlog.go is the append-only writer for the main log, the chat that never
// ends. Kinds: user, agent, tool, echo, work, note, thought, error, shell.
// Every append commits in one store transaction (the log row, its view line
// and, for the kinds a template can line, the message's own node) and then
// wakes the builder.

package hostapi

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/bits"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

const (
	// chatSplitChars: a longer text becomes several sibling rows of about
	// this many characters rather than being cut.
	chatSplitChars = 8_000
	// echoClipChars: a tool result clips to head and tail, this many
	// characters in all.
	echoClipChars = 30_000
)

// chatLog writes the main log of one project store. It owns the builder.
type chatLog struct {
	h    *Host
	tree *memTree
	// reply is the last agent row's seq (Agent), which a turn's stat lands
	// on when the turn ends; -1 for none.
	reply int64
}

// quiesce stops the builder's dispatch and waits for its in-flight builds;
// resume releases it. A reset holds both around the wipe. An error when the
// fence cannot hold: the host is closing, or a build outlived the wait - the
// caller must not cut.
func (c *chatLog) quiesce() error { return c.tree.quiesce() }

func (c *chatLog) resume() { c.tree.resume() }

// newChatLog opens the writer and starts the builder worker on ctx: the
// caller's Close ends both.
func newChatLog(ctx context.Context, h *Host) *chatLog {
	return &chatLog{h: h, tree: newMemTree(ctx, h), reply: -1}
}

// ReleaseBuilder opens the builder's boot gate: the config step ran, so its
// compaction calls can resolve models. A host without a store has no builder.
func (h *Host) ReleaseBuilder() {
	if h.chat != nil {
		h.chat.tree.release()
	}
}

// Close ends the builder: its context cancels, in-flight model calls abort,
// and Close returns once every builder goroutine exited.
func (c *chatLog) Close() { c.tree.Close() }

// User logs the user's words; parts carries the message's images, which
// zoom replays. It returns the first seq it wrote.
func (c *chatLog) User(text, parts string) (int64, error) {
	first, _, err := c.append(store.KindUser, text, "", parts)
	return first, err
}

// Agent logs one of zemi's replies, remembering the last row it wrote: the
// row the turn's stat lands on when the turn ends.
func (c *chatLog) Agent(text string) error {
	_, last, err := c.append(store.KindAgent, text, "", "")
	c.reply = last
	return err
}

// Note logs a memory from before the chat: a chapter mark.
func (c *chatLog) Note(text string) (int64, error) {
	first, _, err := c.append(store.KindNote, text, "", "")
	return first, err
}

// systemNote records a note the chat's transcript draws nothing of: the
// wipe's trace, reachable by zoom and the picker alone.
func (c *chatLog) systemNote(text string) error {
	_, _, err := c.append(store.KindNote, text, `{"system":true}`, "")
	return err
}

// Notice logs a status notice the chat drew: a recap, a goal verdict, a loop
// or workflow event. Detail rides the meta, where the rebuilt row reads it.
func (c *chatLog) Notice(text, detail string) (int64, error) {
	meta := ""
	if detail != "" {
		raw, err := json.Marshal(struct {
			Detail string `json:"detail"`
		}{detail}, json.Deterministic(true))
		if err != nil {
			return -1, err
		}
		meta = string(raw)
	}
	first, _, err := c.append(store.KindNotice, text, meta, "")
	return first, err
}

// Thought logs one settled reasoning stretch: its text and the wall time its
// flush measured, which the rebuilt row and the run digest read back.
func (c *chatLog) Thought(text string, ms int64) error {
	meta, err := json.Marshal(struct {
		MS int64 `json:"ms"`
	}{ms}, json.Deterministic(true))
	if err != nil {
		return err
	}
	if _, err := c.h.DB.AppendLog(store.KindThought, text, string(meta), "", thoughtLeaf(text)); err != nil {
		return err
	}
	c.tree.Wake()
	return nil
}

// Error logs a failed turn's error row: the text it drew and whether the user
// cancelled the turn.
func (c *chatLog) Error(text string, cancelled bool) error {
	meta, err := json.Marshal(struct {
		Cancelled bool `json:"cancelled"`
	}{cancelled}, json.Deterministic(true))
	if err != nil {
		return err
	}
	_, _, err = c.append(store.KindError, text, string(meta), "")
	return err
}

// Shell logs a !command row: the command, its output head and its outcome.
func (c *chatLog) Shell(cmd, out string, ok bool) error {
	meta, err := json.Marshal(struct {
		OK  bool   `json:"ok"`
		Out string `json:"out"`
	}{ok, out}, json.Deterministic(true))
	if err != nil {
		return err
	}
	_, _, err = c.append(store.KindShell, cmd, string(meta), "")
	return err
}

// Work logs a subagent's report or update: the run it reports for, who names
// it ("Counting tests"), the run's agent attribute the rebuilt row's click
// reads its screen from, the report's status word, and the head who and the
// body make. A report too long for one row splits into siblings, each
// labeled, so a continuation reads as the same run's where the split cut it.
func (c *chatLog) Work(run, who, agent, status, text string) (int64, error) {
	meta, err := json.Marshal(struct {
		Run    string `json:"run"`
		Name   string `json:"name"`
		Agent  string `json:"agent,omitempty"`
		Status string `json:"status,omitempty"`
	}{run, who, agent, status}, json.Deterministic(true))
	if err != nil {
		return -1, err
	}
	label := who + " · "
	rows := splitText(text, chatSplitChars-len([]rune(label)))
	for i := 1; i < len(rows); i++ {
		rows[i] = label + rows[i]
	}
	first, _, err := c.appendRows(store.KindWork, rows, string(meta), "")
	return first, err
}

// Tool logs one tool call: its name, the input as sent, and the call id that
// pairs it with its echo across parallel calls of one tool.
func (c *chatLog) Tool(name, input, id string) error {
	meta, err := json.Marshal(struct {
		Tool string `json:"tool"`
		ID   string `json:"id,omitempty"`
	}{name, id}, json.Deterministic(true))
	if err != nil {
		return err
	}
	marked := len(splitText(input, chatSplitChars)) > 1
	if marked {
		meta = []byte(withSplit(string(meta)))
	}
	for _, row := range splitText(input, chatSplitChars) {
		leaf := toolLeaf(name, row)
		if _, err := c.h.DB.AppendLog(store.KindTool, row, string(meta), "", leaf); err != nil {
			return err
		}
	}
	c.tree.Wake()
	return nil
}

// echoRow is the echo row's meta: what a window rebuilt from the log reads
// back that the input alone does not say - the call this result answers, how
// it ended, the change it made and the run or job it handed off to.
type echoRow struct {
	Tool   string `json:"tool"`
	OK     bool   `json:"ok"`
	ID     string `json:"id,omitempty"`
	Status string `json:"status,omitempty"`
	Diff   string `json:"diff,omitempty"`
	Run    string `json:"run,omitempty"`
	Job    string `json:"job,omitempty"`
}

// Echo logs one tool result, clipped to head and tail: the one kind of long
// text that is cut, never split. The call's settled row rides the meta, so
// the rebuilt row draws the diff and the run a live turn drew.
func (c *chatLog) Echo(name string, ok bool, output string, call StreamTool) error {
	status := call.Status
	if status == "" {
		status = statusOf(ok)
	}
	meta, err := json.Marshal(echoRow{
		Tool: name, OK: ok, ID: call.ID, Status: string(status),
		Diff: call.Diff, Run: call.Run, Job: call.Job,
	}, json.Deterministic(true))
	if err != nil {
		return err
	}
	clipped := clipMiddle(output, echoClipChars)
	if _, err := c.h.DB.AppendLog(store.KindEcho, clipped, string(meta), "", echoLeaf(name, ok, clipped)); err != nil {
		return err
	}
	c.tree.Wake()
	return nil
}

// statusOf is the status an echo without a settled call's row reads: the
// result's own ok.
func statusOf(ok bool) CallStatus {
	if ok {
		return CallOK
	}
	return CallErr
}

// replyStat is the readout a finished turn rides its last reply row's meta:
// the model, the turn's wall time, its tokens out, when it ended, and the
// turn's whole thought, which reasoning never being logged leaves to it.
type replyStat struct {
	Model     string `json:"model"`
	TurnMS    int64  `json:"turn_ms"`
	Out       int64  `json:"out"`
	ThoughtMS int64  `json:"thought_ms"`
	Ended     int64  `json:"ended"`
}

// setReplyStat writes the finished turn's readout onto its last reply row,
// which Agent remembered. A turn that wrote no reply (a cancel before any
// text) writes nothing.
func (c *chatLog) setReplyStat(stat TurnStat, thought time.Duration) error {
	if c.reply < 0 {
		return nil
	}
	meta, err := json.Marshal(replyStat{
		Model: stat.Model, TurnMS: stat.ElapsedMS, Out: stat.Out,
		ThoughtMS: thought.Milliseconds(), Ended: stat.Ended,
	}, json.Deterministic(true))
	if err != nil {
		return err
	}
	return c.h.DB.SetLogMeta(c.reply, string(meta))
}

// append logs the rows a text splits into, each with its view line, and
// returns the first and the last seq it wrote.
func (c *chatLog) append(kind store.LogKind, text, meta, parts string) (int64, int64, error) {
	return c.appendRows(kind, splitText(text, chatSplitChars), meta, parts)
}

func (c *chatLog) appendRows(kind store.LogKind, rows []string, meta, parts string) (int64, int64, error) {
	first, last := int64(-1), int64(-1)
	for _, row := range rows {
		if len(rows) > 1 {
			// the chat split a longer message into siblings: the meta marks
			// them, so a rebuild merges what one transcript row holds
			meta = withSplit(meta)
		}
		seq, err := c.h.DB.AppendLog(kind, row, meta, parts, "")
		if err != nil {
			return first, last, err
		}
		if first < 0 {
			first = seq
		}
		last = seq
	}
	c.tree.Wake()
	return first, last, nil
}

// withSplit adds the split mark to a log row's meta: a message the chat
// logged as sibling rows, which a rebuild folds back into one.
func withSplit(meta string) string {
	var m map[string]any
	if json.Unmarshal([]byte(meta), &m) != nil || m == nil {
		m = map[string]any{}
	}
	m["split"] = true
	b, err := json.Marshal(m, json.Deterministic(true))
	if err != nil {
		return meta
	}
	return string(b)
}

// splitMarked reads the split mark withSplit wrote.
func splitMarked(row store.LogEntry) bool {
	var m struct {
		Split bool `json:"split"`
	}
	return json.Unmarshal([]byte(row.MetaJSON), &m) == nil && m.Split
}

// toolLeaf renders a tool call's line mechanically: the tool and the first
// path- or command-shaped value of its input.
// NOTE: sparser than a model line, and the subject is the first plausible
// field, not the right one; merges re-compress these later.
func toolLeaf(name, input string) string {
	line := "tool " + name
	if s := inputSubject(input); s != "" {
		line += " " + s
	}
	return clipBytes(line, nodeBytes)
}

// echoLeaf renders a tool result's line mechanically: the tool, its outcome
// and the head of the output.
// NOTE: sparser than a model line; merges re-compress these later.
func echoLeaf(name string, ok bool, output string) string {
	state := "ok"
	if !ok {
		state = "failed"
	}
	line := "echo " + name + " " + state
	if out := firstText(output, nodeBytes); out != "" {
		line += ": " + out
	}
	return clipBytes(line, nodeBytes)
}

// thoughtLeaf renders a reasoning stretch's line mechanically: its head.
// NOTE: sparser than a model line; merges re-compress these later.
func thoughtLeaf(text string) string {
	return clipBytes("thought: "+firstText(text, nodeBytes), nodeBytes)
}

// inputSubject picks the input field that says what the call acted on.
func inputSubject(input string) string {
	var fields map[string]any
	if err := json.Unmarshal([]byte(input), &fields); err != nil {
		return ""
	}
	for _, key := range []string{"path", "file", "dir", "cmd", "command", "url", "pattern", "query"} {
		if s, ok := fields[key].(string); ok && s != "" {
			return s
		}
	}
	keys := slices.Sorted(maps.Keys(fields))
	for _, key := range keys {
		if s, ok := fields[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// splitText cuts a long text into rows of about chars runes each, at a
// newline when one is near the cut: a long text is never cut, it is logged
// as several messages in a row.
func splitText(text string, chars int) []string {
	if utf8.RuneCountInString(text) <= chars {
		return []string{text}
	}
	r := []rune(text)
	out := make([]string, 0, len(r)/chars+1)
	for len(r) > 0 {
		n := min(len(r), chars)
		if n < len(r) { // more follows: cut at a newline near the end when one is there
			lo := max(0, n-200)
			for j, c := range slices.Backward(r[lo:n]) {
				if c == '\n' {
					n = lo + j + 1
					break
				}
			}
		}
		out = append(out, string(r[:n]))
		r = r[n:]
	}
	return out
}

// clipMiddle clips s to about chars runes, its head and tail with a marker
// between: a tool result keeps its start and its end.
func clipMiddle(s string, chars int) string {
	r := []rune(s)
	if len(r) <= chars {
		return s
	}
	head := chars / 2
	return string(r[:head]) + "\n...\n" + string(r[len(r)-(chars-head):])
}

// firstText is the text's first line, at most chars runes: the error a
// failed result names sits at its head.
func firstText(s string, chars int) string {
	head, _, _ := strings.Cut(s, "\n")
	return clipBytes(head, chars)
}

// clipBytes cuts s to at most bytes UTF-8 bytes without splitting a rune.
func clipBytes(s string, bytes int) string {
	if len(s) <= bytes {
		return s
	}
	cut := bytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// viewLines are the text parts one view message carries: one part per this
// many view lines, so a turn's parts extend the last turn's whole.
const viewLinesPerPart = 4

// renderLine renders one view line as the model reads it: "id+n|text", the
// placeholder's when the node is not built yet (text empty).
func renderLine(l, i int64, text string) string {
	if text == "" {
		text = placeholder
	}
	return nodeID{l, i}.id() + "|" + strings.ReplaceAll(text, "\n", " ")
}

// isViewMessage reports whether a history slice opens on the chat view: one
// user message whose first part carries the view's opening tag.
func isViewMessage(msgs []fantasy.Message) bool {
	if len(msgs) == 0 || msgs[0].Role != fantasy.MessageRoleUser || len(msgs[0].Content) == 0 {
		return false
	}
	part, ok := fantasy.AsMessagePart[fantasy.TextPart](msgs[0].Content[0])
	return ok && strings.HasPrefix(part.Text, "<chat>")
}

// viewMessage is the fresh turn's opening message: the persisted view, one
// text part per viewLinesPerPart lines, oldest first. The mark sits on the
// newest complete block that has a part after it - a complete last block
// still carries the closing tag, which the next line moves - so every marked
// part keeps its bytes while the view only grows at its end. ok is false
// while the log is empty.
func (c *chatLog) viewMessage() (msg fantasy.Message, ok bool) {
	lines, err := c.h.DB.ViewLines()
	if err != nil {
		c.h.Log.Warn("view not read", "origin", "agent", "error", err)
		return fantasy.Message{}, false
	}
	if len(lines) == 0 {
		return fantasy.Message{}, false
	}
	texts := make([]string, 0, (len(lines)+viewLinesPerPart-1)/viewLinesPerPart)
	for start := 0; start < len(lines); start += viewLinesPerPart {
		var b strings.Builder
		for _, ln := range lines[start:min(start+viewLinesPerPart, len(lines))] {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(renderLine(ln.L, ln.I, ln.Text))
		}
		texts = append(texts, b.String())
	}
	texts[0] = "<chat>\n" + texts[0]
	texts[len(texts)-1] += "\n</chat>"
	for i, text := range texts {
		part := fantasy.TextPart{Text: text}
		if len(texts) >= 2 && i == len(texts)-2 {
			part.ProviderOptions = agent.WithCacheControl(part.ProviderOptions)
		}
		msg.Content = append(msg.Content, part)
	}
	msg.Role = fantasy.MessageRoleUser
	return msg, true
}

// zoomArgs is the zoom tool's input and its schema source.
type zoomArgs struct {
	ID   int64 `json:"id" desc:"the line's first message id, the number before the + in id+n"`
	N    int64 `json:"n,omitempty" desc:"how many messages the line covers, a power of 2; 1 gives the message whole"`
	Page int   `json:"page,omitempty" desc:"with n = 1: the 0-based page of a message too long for one result"`
}

// zoomPageChars bounds one zoom(id, 1) page.
const zoomPageChars = 8_000

// zoomTool opens the view's lines: node(id, n) into its two children, and a
// message whole.
type zoomTool struct {
	tools.NoProviderOptions
	h *Host
}

func (h *Host) zoomTool() fantasy.AgentTool { return &zoomTool{h: h} }

func (t *zoomTool) Info() fantasy.ToolInfo {
	props, req := tools.ArgsOf(zoomArgs{})
	return fantasy.ToolInfo{
		Name:        string(tools.ToolZoom),
		Description: "Open a line of the chat view. zoom(id, n) with n a power of 2 gives the two lines id+n was merged from; zoom(id, 1) gives message id whole, images included, paged when it is long: pass page for the next one. This is the only way to read past a summary.",
		Parameters:  props,
		Required:    req,
		Parallel:    true,
	}
}

func (t *zoomTool) Run(ctx context.Context, call fantasy.ToolCall) (reply fantasy.ToolResponse, err error) {
	var in zoomArgs
	if err := decodeInput(t.Info(), call.Input, &in); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if resp, stop := t.h.preCheck(string(tools.ToolZoom), call.Input); stop {
		return resp, nil
	}
	defer t.h.postNotify(ctx, t.h.ScratchDir(), string(tools.ToolZoom), call.Input, &reply, time.Now())
	reply, err = t.h.zoom(in)
	return reply, err
}

// zoom answers one zoom call; every refusal is a tool error, never a panic.
func (h *Host) zoom(in zoomArgs) (fantasy.ToolResponse, error) {
	if h.DB == nil {
		return fantasy.NewTextErrorResponse("no chat log in this boot"), nil
	}
	if in.N == 0 {
		in.N = 1
	}
	if in.N < 0 {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("n is %d: it must be positive", in.N)), nil
	}
	if in.N&(in.N-1) != 0 { // "about 3" reads as the next power of two up
		in.N = 1 << bits.Len64(uint64(in.N))
	}
	count, err := h.DB.LogCount()
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	// in.ID > count-in.N, not in.ID+in.N > count: the sum can overflow
	if in.ID < 0 || in.N > count || in.ID > count-in.N {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("no line %d+%d in a chat of %d messages", in.ID, in.N, count)), nil
	}
	if in.N > 1 {
		shift := uint(bits.Len64(uint64(in.N)) - 1)
		l, i := int64(shift), in.ID>>shift
		return fantasy.NewTextResponse(strings.Join([]string{
			zoomChild(h.DB, l-1, i*2),
			zoomChild(h.DB, l-1, i*2+1),
		}, "\n")), nil
	}
	row, err := h.DB.LogAt(in.ID)
	if errors.Is(err, store.ErrNotFound) {
		// the row was wiped between the count and here (a reset)
		return fantasy.NewTextErrorResponse(fmt.Sprintf("no line %d+%d in a chat of %d messages", in.ID, in.N, count)), nil
	}
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if row.PartsJSON != "" {
		return zoomImages(row), nil
	}
	head := string(row.Kind)
	if name := rowTool(row); name != "" {
		head += " " + name
	}
	if row.Kind == store.KindWork {
		if run := workRun(row); run != "" {
			return h.zoomRun(run, in.Page, row.Text)
		}
	}
	text := row.Text
	if pages := (utf8.RuneCountInString(text) + int(zoomPageChars) - 1) / int(zoomPageChars); in.Page >= pages {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("message %d has %d pages", in.ID, pages)), nil
	}
	return fantasy.NewTextResponse(pageText(head, text, in.ID, in.Page)), nil
}

// pageText is one message as a zoom(id, 1) result: its head, its text, paged
// when it is long.
func pageText(head, text string, id int64, page int) string {
	r := []rune(text)
	pages := (len(r) + int(zoomPageChars) - 1) / int(zoomPageChars)
	if page > 0 {
		at := int64(page) * zoomPageChars
		if at >= int64(len(r)) {
			return fmt.Sprintf("message %d has %d pages", id, pages)
		}
		return head + ": " + string(r[at:min(at+zoomPageChars, int64(len(r)))])
	}
	if pages > 1 {
		text = string(r[:zoomPageChars]) + fmt.Sprintf("\n(page 1 of %d; zoom(%d, 1, page: 1) for the next)", pages, id)
	}
	return head + ": " + text
}

// workRun is the run a work row reports for, from its meta; "" for none.
func workRun(row store.LogEntry) string {
	if row.Kind != store.KindWork {
		return ""
	}
	var meta struct {
		Run string `json:"run"`
	}
	if json.Unmarshal([]byte(row.MetaJSON), &meta) != nil {
		return ""
	}
	return meta.Run
}

// zoomRun is zoom(id, 1) on a work message: the run it names, its transcript
// paged by the same 8k characters a long message gets. A run whose agents
// were all replayed from an earlier journal has no transcript of its own;
// the work row's report stands in for it.
func (h *Host) zoomRun(run string, page int, own string) (fantasy.ToolResponse, error) {
	rows, err := h.DB.Messages(run)
	if errors.Is(err, store.ErrNotFound) {
		return fantasy.NewTextResponse(workGone), nil
	}
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	_, tr := historyFrom(rows)
	if len(tr) == 0 {
		return fantasy.NewTextResponse(pageText("run "+run, own, -1, page)), nil
	}
	var b strings.Builder
	for _, m := range tr {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		text := m.Text
		if m.Kind != "" {
			text = string(m.Kind) + " " + text
		}
		b.WriteString(sanitize.String(clipRunes(strings.TrimSpace(text), 2000)))
	}
	return fantasy.NewTextResponse(pageText("run "+run, b.String(), -1, page)), nil
}

// workGone is what zoom says of a work row whose run's rows are gone.
const workGone = "the run's transcript is gone from this store"

// rowTool is the tool a tool or echo row names, from its meta; "" for the
// other kinds.
func rowTool(row store.LogEntry) string {
	if row.Kind != store.KindTool && row.Kind != store.KindEcho {
		return ""
	}
	var meta struct {
		Tool string `json:"tool"`
	}
	if json.Unmarshal([]byte(row.MetaJSON), &meta) != nil {
		return ""
	}
	return meta.Tool
}

// zoomChild renders one child line of node(l,i), a placeholder when unbuilt.
func zoomChild(db *store.DB, l, i int64) string {
	text := ""
	if n, err := db.NodeAt(l, i); err == nil {
		text = n.Text
	}
	return renderLine(l, i, text)
}

// zoomImages replays a message's stored images. NOTE: one image per zoom
// result is what a tool response carries; a multi-image message replays its
// first, and the rest wait for a result that can carry more.
func zoomImages(row store.LogEntry) fantasy.ToolResponse {
	var imgs []fantasy.FilePart
	if err := json.Unmarshal([]byte(row.PartsJSON), &imgs); err != nil {
		return fantasy.NewTextErrorResponse("message " + strconv.FormatInt(row.Seq, 10) + "'s parts do not decode")
	}
	for _, f := range imgs {
		if f.MediaType != "" {
			return fantasy.NewImageResponse(f.Data, f.MediaType)
		}
	}
	return fantasy.NewTextResponse(string(row.Kind) + ": " + row.Text)
}

// dateArgs is the date tool's input and its schema source.
type dateArgs struct {
	ID int64 `json:"id" desc:"the message id, the number before the + in id+n"`
}

// dateTool gives a log message's time.
type dateTool struct {
	tools.NoProviderOptions
	h *Host
}

func (h *Host) dateTool() fantasy.AgentTool { return &dateTool{h: h} }

func (t *dateTool) Info() fantasy.ToolInfo {
	props, req := tools.ArgsOf(dateArgs{})
	return fantasy.ToolInfo{
		Name:        string(tools.ToolDate),
		Description: "The date and time a chat message was written.",
		Parameters:  props,
		Required:    req,
		Parallel:    true,
	}
}

func (t *dateTool) Run(ctx context.Context, call fantasy.ToolCall) (reply fantasy.ToolResponse, err error) {
	var in dateArgs
	if err := decodeInput(t.Info(), call.Input, &in); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if resp, stop := t.h.preCheck(string(tools.ToolDate), call.Input); stop {
		return resp, nil
	}
	defer t.h.postNotify(ctx, t.h.ScratchDir(), string(tools.ToolDate), call.Input, &reply, time.Now())
	if t.h.DB == nil {
		return fantasy.NewTextErrorResponse("no chat log in this boot"), nil
	}
	row, err := t.h.DB.LogAt(in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("no message %d", in.ID)), nil
	}
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	return fantasy.NewTextResponse(row.CreatedAt), nil
}

// chatCommands are the eternal chat's own: /note, one line into the log, a
// chapter mark, and /reset, the wipe that starts the log over.
func (h *Host) chatCommands() []*Command {
	return []*Command{{
		Name: "note", NArgs: NArgsSome, Desc: "add a note to the chat's memory",
		GoFn: func(h *Host, args string, confirm bool) error {
			if strings.TrimSpace(args) == "" {
				return Usagef("usage: /note <text>")
			}
			if err := h.Note(args); err != nil {
				return err
			}
			h.Notify(slog.LevelInfo, "Note added", "it joins the chat's memory")
			return nil
		},
	}, {
		Name: "reset", NArgs: NArgsOptional, Desc: "wipe this chat's log and summary tree and start it over",
		GoFn: func(h *Host, args string, confirm bool) error {
			if !h.writerFlag() {
				return errNotWriter
			}
			_, yes := ConfirmArgs(args, confirm)
			if !confirm {
				return h.ResetChat(yes)
			}
			// the armed key's press: the wipe leaves the command path at once,
			// so a builder call wedged under the quiesce never sits on the UI's
			// worker; the toast names the outcome
			h.GoSafe("chat reset", func() {
				if err := h.ResetChat(true); err != nil {
					h.CommandFailed("reset", err)
				}
			})
			return nil
		},
	}}
}

// Note appends one note-kind log row. It is an append, so it works while a
// turn runs as well as while the agent is idle.
func (h *Host) Note(text string) error {
	if h.chat == nil {
		return errors.New("no chat log in this boot")
	}
	text = strings.TrimSpace(text)
	first, err := h.chat.Note(text)
	if err != nil {
		return err
	}
	// the chapter mark joins the live transcript at once; a rebuilt window
	// reads the same row from the log
	if first >= 0 {
		h.appendTranscript(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: messageHead(text), Seq: first})
	}
	return nil
}

// notice appends a status notice to the log and the live transcript, so a
// resume reads back the row the chat drew: recaps, goal verdicts, loop and
// workflow events. An append, like Note; the row draws the first line, as
// the rebuilt window will.
func (h *Host) notice(text, detail string) {
	text = strings.TrimSpace(text)
	m := ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: messageHead(text), Detail: detail}
	if h.chat != nil {
		first, err := h.chat.Notice(text, detail)
		if err != nil {
			h.logAppendFailed(err)
		} else {
			m.Seq = first
		}
	}
	h.AppendChat(m)
}
