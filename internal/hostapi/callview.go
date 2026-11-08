// callview.go opens the view a chat row stands for: a tool call or a
// thought, with its command, its diff or its thinking drawn in the form it
// is written in. The chat's rows name the row they drew; a hidden /call
// resolves it against the transcript on screen, the live turn or the run
// the chat shows.

package hostapi

import (
	"cmp"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

func (h *Host) callCommands() []*Command {
	return []*Command{{
		Name: "call", NArgs: NArgsSome, Hidden: true, Desc: "open a chat row's own view (the chat's rows do this when clicked)",
		GoFn: func(h *Host, args string, confirm bool) error {
			args = strings.TrimSpace(args)
			if seq, err := strconv.ParseInt(strings.TrimPrefix(args, "#"), 10, 64); err == nil && strings.HasPrefix(args, "#") {
				return h.openLogCall(seq)
			}
			n, err := strconv.Atoi(args)
			if err != nil {
				return Usagef("/call <row> (a chat row's number from the top, 1-based; negative for the live turn's calls, #seq for a logged one)")
			}
			return h.openCall(n)
		},
	}}
}

// openCall opens the view of the row at n, which is 1-based into the
// transcript the chat draws and negative for the live turn's calls (-1 is
// its first). A call still running refreshes under it until it ends.
func (h *Host) openCall(n int) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	row, took, ok := h.chatRow(n)
	if !ok {
		return Usagef("the chat shows no row %d", n)
	}
	if row.Kind != RowTool && row.Kind != RowReasoning {
		return Usagef("row %d is not a call", n)
	}
	h.levelMu.Lock()
	h.callRow, h.callID = n, row.CallID
	h.levelMu.Unlock()
	ui.OpenOverlay(h.callView(n, row, took))
	// a live view follows the turn as it is written; a settled one is put
	// right by the transcript hook when the rows it steps along land
	if n < 0 || row.Status == CallRun || row.Status == CallWait {
		h.watchCall(n)
	}
	return nil
}

// refreshCall rebuilds the call view open, if any: the rows it steps along
// and the status it shows come from a transcript that just grew, and a call
// that settled after the view opened is a step target at once.
func (h *Host) refreshCall() {
	h.levelMu.Lock()
	n := h.callRow
	h.levelMu.Unlock()
	ui := h.hooks()
	if ui == nil || n == 0 {
		return
	}
	row, took, ok := h.callRowAt(n)
	if !ok {
		return
	}
	ui.RefreshOverlay("call:"+strconv.Itoa(n), func() Overlay { return h.callView(n, row, took) })
}

// callRowAt is the row a call view shows: a live call while its tool is in
// the stream, and the row it settles at once the turn lands. It reads the
// transcript alone, so a turn's flush, which holds the agent lock, can call
// it: the row a view stepped to is never a live one there.
func (h *Host) callRowAt(n int) (ChatMessage, time.Duration, bool) {
	if n > 0 {
		return h.chatRow(n)
	}
	if m := h.settledRow(n); m > 0 {
		return h.chatRow(m)
	}
	return ChatMessage{}, 0, false
}

// settledRow is the transcript row the live call at n landed at, 0 while it
// still runs or the view has moved on: a turn settles its calls under the
// agent lock, so this reads the transcript and the id the view opened on.
func (h *Host) settledRow(n int) int {
	h.levelMu.Lock()
	id, tracked := h.callID, h.callRow
	h.levelMu.Unlock()
	// a live call is a session turn's: a run on screen has none, and a turn
	// settles under the agent lock, which this path must never take
	if id == "" || tracked != n || h.viewedRun() != nil {
		return 0
	}
	for i, m := range h.Session().Transcript {
		if m.CallID == id {
			return i + 1
		}
	}
	return 0
}

// streamRow is a live call as the chat draws it, with how long it has run.
// A waiting call has not started: it gets no time, not an epoch.
func streamRow(t StreamTool) (ChatMessage, time.Duration) {
	var took time.Duration
	if !t.Started.IsZero() {
		took = time.Since(t.Started)
	}
	return ChatMessage{
		Kind: RowTool, CallID: t.ID, Tool: tools.ToolName(t.Name), Text: t.Input, Label: t.Label,
		Prompt: steerPrompt(t.Name, t.Raw),
		Status: t.Status, Detail: t.Detail, Tail: t.Tail, Run: t.Run, Diff: t.Diff, Output: t.Output,
	}, took
}

// chatRow is the row the chat draws at n: the live turn's calls, the rows of
// the run the chat shows, else the session's. took is how long a live call
// has run, 0 for a settled one, since no row records that.
func (h *Host) chatRow(n int) (row ChatMessage, took time.Duration, ok bool) {
	if n < 0 {
		s := h.AgentStream()
		i := -n - 1
		if i >= len(s.Tools) {
			return ChatMessage{}, 0, false
		}
		t := s.Tools[i]
		row, took := streamRow(t)
		return row, took, true
	}
	tr := h.Session().Transcript
	if run := h.viewedRun(); run != nil {
		tr = run.transcript()
	}
	if n < 1 || n > len(tr) {
		return ChatMessage{}, 0, false
	}
	return tr[n-1], 0, true
}

// watchCall keeps an open call view current while the call runs: its output
// and its mark change in place. A live row's index names another call once
// the next turn starts, so the ticker stops with the turn. One watcher
// serves the open view: the next watchCall supersedes it, and a watcher the
// host no longer serves stops, so stepping through live calls never piles
// watchers up whose clustered ticks blink the view's timer.
func (h *Host) watchCall(n int) {
	h.levelMu.Lock()
	h.callServe = n
	h.levelMu.Unlock()
	h.overlayTicker("call view", nil, func() bool { return h.callWatchTick(n) })
}

// callWatchTick is the live call view watcher's tick: it rebuilds the view
// while it is the one the host serves and reports whether to keep ticking.
func (h *Host) callWatchTick(n int) bool {
	h.levelMu.Lock()
	live := h.callServe == n
	h.levelMu.Unlock()
	if !live {
		return false
	}
	row, took, ok := h.callViewRow(n)
	if !ok || n < 0 && !h.AgentActive() {
		return false
	}
	if ui := h.hooks(); ui != nil {
		ui.RefreshOverlay("call:"+strconv.Itoa(n), func() Overlay { return h.callView(n, row, took) })
	}
	// a live view keeps up with the run it steps along; a settled one
	// has nothing to say until the transcript grows, and refreshCall
	// takes it then
	return n < 0 || row.Status == CallRun || row.Status == CallWait
}

// callViewRow is the row the open call view shows: the call it opened for,
// found by its id in the live stream - a flush prunes settled calls out of
// the stream, so the index the view opened at names a later call afterwards
// - and, once it settles, at the transcript row it landed at. The stream
// read takes the agent lock: this runs on the watcher, never under it.
func (h *Host) callViewRow(n int) (row ChatMessage, took time.Duration, ok bool) {
	if n > 0 {
		return h.chatRow(n)
	}
	h.levelMu.Lock()
	id := h.callID
	h.levelMu.Unlock()
	if id == "" { // an unnamed call (a seeded stream): the index is all there is
		return h.chatRow(n)
	}
	s := h.AgentStream()
	if i := slices.IndexFunc(s.Tools, func(t StreamTool) bool { return t.ID == id }); i >= 0 {
		row, took = streamRow(s.Tools[i])
		return row, took, true
	}
	if m := h.settledRow(n); m > 0 {
		return h.chatRow(m)
	}
	return ChatMessage{}, 0, false
}

// _callTitles name a call's view by what it is, not by the verb its row reads
// with; any other tool reads as its own name's words.
var _callTitles = map[tools.ToolName]string{
	tools.ToolShell: "Command", tools.ToolEdit: "Edit", tools.ToolWrite: "Write", tools.ToolRead: "Read",
	tools.ToolGrep: "Search", tools.ToolGlob: "Find", tools.ToolSkill: "Skill",
	tools.ToolPlan: "Plan", tools.ToolLua: "Script", tools.ToolTodos: "Todos",
}

// _callStates says where a call stands: the word its view reads and the mark
// its row and view wear, one entry per state.
var _callStates = map[CallStatus]struct {
	word string
	mark MarkKind
}{
	CallWait:      {"waiting", tools.MarkWait},
	CallRun:       {"running", tools.MarkSpin},
	CallOK:        {"ok", tools.MarkOK},
	CallWarn:      {"warned", tools.MarkWarn},
	CallErr:       {"failed", tools.MarkErr},
	CallStopped:   {"stopped", tools.MarkStop},
	CallBG:        {"in the background", tools.MarkBG},
	CallCancelled: {"cancelled", tools.MarkStop},
}

// _callArg names what a call was given, so the facts table says what it acted
// on before the body says what came back.
var _callArg = map[tools.ToolName]string{
	tools.ToolRead: "file", tools.ToolEdit: "file", tools.ToolWrite: "file", tools.ToolGrep: "pattern",
	tools.ToolGlob: "pattern", tools.ToolSkill: "skill",
	tools.ToolTodos: "what",
}

// steerPrompt is the message an agent call sent a run, whole: the row's
// summary keeps 80 runes of it. Empty for a call that started a run, whose
// prompt is its task, or one that only stopped or waited.
func steerPrompt(tool, raw string) string {
	if tools.ToolName(tool) != tools.ToolAgent {
		return ""
	}
	var in struct {
		To     string `json:"send_to"`
		Prompt string `json:"prompt"`
	}
	if json.Unmarshal([]byte(raw), &in) != nil || in.To == "" {
		return ""
	}
	return strings.TrimSpace(in.Prompt)
}

// callInput is the input a call acted on, what copying a call row means to
// take: the command a shell or lua call ran, the message an agent call sent a
// run, else the file, pattern or name its argument names. Empty when the call
// took nothing worth copying.
func callInput(m ChatMessage) string {
	if m.Tool == tools.ToolAgent {
		return m.Prompt
	}
	if m.Text == "" {
		return ""
	}
	if m.Tool == tools.ToolShell || m.Tool == tools.ToolLua {
		return m.Text
	}
	if _, ok := _callArg[m.Tool]; ok {
		return m.Text
	}
	return ""
}

// callView is one row's view: the mark and the facts of the call, then its
// content in parts, each drawn the way its text is written.
func (h *Host) callView(n int, m ChatMessage, took time.Duration) Overlay {
	o := h.callBody(m, took)
	o.ID = "call:" + strconv.Itoa(n)
	o.Prev, o.Next = h.callStep(n)
	return o
}

// logCallView is one logged call's view: the chat's own call view over a row
// the log holds, its steps walking the log's rail around the row.
func (h *Host) logCallView(seq int64, m ChatMessage) Overlay {
	o := h.callBody(m, 0)
	o.ID = "call:#" + strconv.FormatInt(seq, 10)
	o.Prev, o.Next = h.logCallStep(seq)
	return o
}

// callBody builds what a call's view says: its title, mark and live flag,
// the facts of the call and its content in parts.
func (h *Host) callBody(m ChatMessage, took time.Duration) Overlay {
	o := Overlay{
		Title: callTitle(m), Origin: lua.OriginBuiltin,
		Mark: _callStates[m.Status].mark, Live: m.Status == CallRun || m.Status == CallWait,
	}
	var parts []BodyPart
	fields := []Field{{Name: "status", Value: _callStates[m.Status].word}}
	if m.Kind == RowReasoning {
		parts = append(parts, BodyPart{Text: m.Text, Lang: "markdown"})
		if m.Stat != nil && m.Stat.ThoughtMS > 0 {
			fields = append(fields, Field{Name: "time", Value: fmtDuration(time.Duration(m.Stat.ThoughtMS) * time.Millisecond)})
		}
	} else {
		if took >= time.Second {
			fields = append(fields, Field{Name: "time", Value: fmtDuration(took)})
		}
		if m.Label != "" {
			fields = append(fields, Field{Name: "about", Value: m.Label})
		}
		if in := callInput(m); in != "" {
			switch m.Tool {
			case tools.ToolShell, tools.ToolLua:
				// a command is code, so it heads the body rather than the facts,
				// and sinks into the well its answer lifts out of
				parts = append(parts, BodyPart{Text: in, Lang: "bash", Fill: FillCanvas})
			case tools.ToolAgent:
				// a message to a run is prose it reads, so it heads the body
				// whole where the row's summary clips it
				parts = append(parts, BodyPart{Text: in, Lang: "markdown"})
			default:
				fields = append(fields, Field{Name: _callArg[m.Tool], Value: in})
			}
		}
		if m.Diff != "" {
			parts = append(parts, BodyPart{Text: m.Diff, Lang: "diff"})
		}
		switch out := cmp.Or(h.skillBody(m), callOutput(m), m.Tail); {
		case out != "":
			// a skill answers in markdown, on its own surface; anything else
			// reads in the well the command opened
			if m.Tool == tools.ToolSkill {
				parts = append(parts, BodyPart{Text: out, Lang: "markdown"})
			} else {
				parts = append(parts, BodyPart{Text: out, Fill: FillWell})
			}
		case m.Detail != "":
			// a running call with no output yet says what it is doing
			parts = append(parts, BodyPart{Text: m.Detail, Fill: FillWell})
		}
	}
	if m.Job != "" {
		fields = append(fields, Field{Name: "job", Value: m.Job})
		o.Keys = append(o.Keys, OverlayKey{Key: "o", Desc: "open job", Run: "jobs view " + m.Job})
	}
	o.Fields, o.Parts = fields, parts
	for _, p := range parts {
		if p.Text != "" {
			if o.Body != "" {
				o.Body += "\n\n"
			}
			o.Body += p.Text
		}
	}
	return o
}

// openLogCall opens the view of one logged call: the row a loaded page of
// the log row's seq names, drawn like the chat's own call view.
func (h *Host) openLogCall(seq int64) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	m, ok := h.logCallRow(seq)
	if !ok {
		return Usagef("no call at message %d", seq)
	}
	ui.OpenOverlay(h.logCallView(seq, m))
	return nil
}

// logCallRow is the logged call at seq: the row the log names, its echo
// merged in the way the chat's window reads them.
func (h *Host) logCallRow(seq int64) (ChatMessage, bool) {
	if h.DB == nil {
		return ChatMessage{}, false
	}
	rows, err := h.DB.LogFrom(seq, 16)
	if err != nil || len(rows) == 0 || rows[0].Seq != seq {
		return ChatMessage{}, false
	}
	for _, m := range logRows(h.relPath, rows) {
		if m.Seq == seq {
			if m.Kind != RowTool && m.Kind != RowReasoning {
				return ChatMessage{}, false
			}
			return m, true
		}
	}
	return ChatMessage{}, false
}

// logCallStep names the logged calls around seq: the run of call, echo and
// thought rows the log holds the rail in, walked the way a windowed call's
// view walks its transcript rail.
func (h *Host) logCallStep(seq int64) (prev, next string) {
	if h.DB == nil {
		return "", ""
	}
	step := func(row store.LogEntry) string {
		switch row.Kind {
		case store.KindTool, store.KindThought:
			return "call #" + strconv.FormatInt(row.Seq, 10)
		case store.KindEcho:
			// the result rides the call it answers: the rail walks past it
		default:
			return "" // a message of its own ends the rail
		}
		return ""
	}
	// an echo pairs with the call before it; anything else ends the rail
	for s := seq - 1; s >= 0; s-- {
		row, err := h.DB.LogAt(s)
		if err != nil {
			break
		}
		if cmd := step(row); cmd != "" {
			prev = cmd
			break
		}
		if row.Kind != store.KindEcho {
			break
		}
	}
	for s := seq + 1; ; s++ {
		row, err := h.DB.LogAt(s)
		if err != nil {
			break
		}
		if cmd := step(row); cmd != "" {
			next = cmd
			break
		}
		if row.Kind != store.KindEcho {
			break
		}
	}
	return prev, next
}

// skillBody is what a skill call answers in its view: the skill's own file
// whole, formatted as markdown, where the row keeps only the first and last
// lines of the result. Empty for any other call, or a skill gone since.
func (h *Host) skillBody(m ChatMessage) string {
	if m.Tool != tools.ToolSkill {
		return ""
	}
	name := strings.TrimSpace(m.Text)
	s, ok := h.skillsReg.get(name)
	if !ok { // a project's skill or a recipe lives outside the stored table
		for _, p := range h.projectOnlySkills() {
			if p.Name == name {
				s, ok = p, true
				break
			}
		}
	}
	if !ok {
		return ""
	}
	text, err := tools.SkillText(tools.SkillInfo{Name: s.Name, Path: s.Dir, File: s.File, Root: s.Root, Data: s.Data})
	if err != nil {
		return ""
	}
	_, body, found := strings.Cut(text, "\n\n")
	if !found {
		return text
	}
	return body
}

// callStep names the views around row n in its rail: the run of thinking and
// call rows the chat draws as one unit between two messages, so ctrl+j and
// ctrl+k walk it. The turn settles its calls into the transcript's tail while
// it still writes the later ones, so the settled rows and the live calls are
// one rail and a step crosses between them. Every rail row steps, a run
// digest like any other: a step lands on the view the row itself opens.
func (h *Host) callStep(n int) (prev, next string) {
	tr := h.viewedTranscript()
	s := h.AgentStream()
	// a shown run has no live stream of its own, so the live calls are steps
	// of the session's rail alone
	cross := h.viewedRun() == nil && s.Active
	rail := func(i int) bool { return tr[i].Kind == RowTool || tr[i].Kind == RowReasoning }
	step := func(i int) string {
		if !rail(i) {
			return ""
		}
		if tr[i].Run != "" {
			return runStepView(string(tr[i].Tool), tr[i].Run)
		}
		return "call " + strconv.Itoa(i+1)
	}
	back := func(from int) string {
		for i := from; i >= 0 && rail(i); i-- {
			if cmd := step(i); cmd != "" {
				return cmd
			}
		}
		return ""
	}
	live := func(j int) string {
		if j < 0 || j >= len(s.Tools) {
			return ""
		}
		if t := s.Tools[j]; t.Run != "" {
			return runStepView(t.Name, t.Run)
		}
		return "call " + strconv.Itoa(-j-1)
	}
	// the live turn's calls are named by negative rows, -1 its first: a view
	// of one steps along the run still being written, not a snapshot of
	// whatever had settled when it opened
	if n < 0 {
		if k := -n; k <= len(s.Tools) {
			if prev = live(k - 2); prev == "" && cross {
				prev = back(len(tr) - 1)
			}
			return prev, live(k)
		}
		// the turn landed since the view opened: it steps along the rows the
		// calls settled at
		if m := h.settledRow(n); m > 0 {
			return h.callStep(m)
		}
		return "", ""
	}
	if n < 1 || n > len(tr) {
		return "", ""
	}
	prev = back(n - 2)
	var i int
	for i = n; i < len(tr) && rail(i); i++ {
		if cmd := step(i); cmd != "" {
			next = cmd
			break
		}
	}
	// the walk ran to the transcript's end: the rail runs on into the calls
	// the turn is still writing
	if i == len(tr) && cross {
		next = live(0)
	}
	return prev, next
}

// runStepView names the view a run's row opens, the one its click opens: a
// workflow's its view, an agent's its preview. A step lands there, so the
// walk shows what the row shows anywhere else.
func runStepView(tool, run string) string {
	if tools.ToolName(tool) == tools.ToolWorkflow {
		return "workflows view " + run
	}
	return "agents show " + run
}

// railStep is the walk around the transcript row that names run: the prev and
// next step commands its view steps with, "" when no row of the transcript
// the chat draws holds it - a run's view walks only where it stands.
func (h *Host) railStep(run string) (prev, next string) {
	tr := h.viewedTranscript()
	for i, m := range tr {
		if m.Kind == RowTool && m.Run == run {
			return h.callStep(i + 1)
		}
	}
	if j := slices.IndexFunc(h.AgentStream().Tools, func(t StreamTool) bool { return t.Run == run }); j >= 0 {
		return h.callStep(-j - 1)
	}
	return "", ""
}

// callTitle names the view: a thought is a thought, a question it asks, and
// every other call what it is.
func callTitle(m ChatMessage) string {
	switch {
	case m.Kind == RowReasoning:
		return "Thought"
	case m.Tool == tools.ToolQuestion:
		return "Question"
	}
	if t, ok := _callTitles[m.Tool]; ok {
		return t
	}
	return upperFirst(strings.ReplaceAll(strings.TrimPrefix(string(m.Tool), _mcpToolPrefix), "_", " "))
}

// callOutput is what the call answered, as its view shows it: a question's
// answers read as a table under their questions, and a file call's diff is
// the answer it gives, so it stands alone.
func callOutput(m ChatMessage) string {
	if m.Tool == tools.ToolQuestion {
		var b strings.Builder
		for i, l := range strings.Split(strings.TrimRight(m.Output, "\n"), "\n") {
			fields := strings.SplitN(l, "\t", 3)
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(fields[0])
			if len(fields) > 1 {
				b.WriteString("   " + fields[1])
			}
			if len(fields) > 2 && fields[2] != "" {
				b.WriteString("\nnote: " + fields[2])
			}
		}
		return b.String()
	}
	if m.Diff != "" {
		return ""
	}
	return m.Output
}

// viewedTranscript is the chat's rows: the session's, or the shown run's,
// read the way a render reads them.
func (h *Host) viewedTranscript() []ChatMessage {
	sess, stream := h.liveView()
	if run := h.viewedRun(); run != nil {
		rows, _, _ := run.view(stream)
		return rows
	}
	return sess.Transcript
}
