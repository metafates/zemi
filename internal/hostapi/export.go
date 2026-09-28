// export.go is what a session can be copied out of: /export writes the
// conversation as markdown, /copy puts a reply or a code block on the
// clipboard, /fork continues it in a new session so the original stays as
// it was, and /usage reports what it cost. It also caches the todo list the
// chat widget shows.
package hostapi

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
)

// CopyReply copies the live session's last reply as the model wrote it (a
// drag copies rendered, wrapped screen rows). args is "" (a picker when the
// reply has code blocks), "reply", "code" (every block) or "code <n>".
func (h *Host) CopyReply(args string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	sid := h.AgentSessionID()
	if sid == "" || h.DB == nil {
		h.Notify(logging.Warn, "Nothing to copy", "")
		return nil
	}
	rows, err := h.DB.Messages(sid)
	if err != nil {
		return err
	}
	history, _ := historyFrom(rows)
	text := sanitize.String(lastAssistantText(history))
	if text == "" {
		h.Notify(logging.Warn, "Nothing to copy", "")
		return nil
	}
	blocks := codeBlocks(text)
	f := strings.Fields(args)
	switch {
	case len(f) == 0 && len(blocks) == 0, len(f) == 1 && f[0] == "reply":
		ui.Copy(text)
	case len(f) == 0:
		ui.OpenOverlay(copyPicker(blocks))
	case f[0] != "code" || len(f) > 2:
		return Usagef("usage: /copy [reply | code [n]]")
	case len(blocks) == 0:
		return Usagef("the last reply has no code blocks")
	case len(f) == 1:
		parts := make([]string, len(blocks))
		for i, b := range blocks {
			parts[i] = strings.TrimSuffix(b.text, "\n")
		}
		ui.Copy(strings.Join(parts, "\n\n") + "\n")
	default:
		n, err := strconv.Atoi(f[1])
		if err != nil || n < 1 || n > len(blocks) {
			return Usagef("no code block %s: the last reply has %d", f[1], len(blocks))
		}
		ui.Copy(blocks[n-1].text)
	}
	return nil
}

// copyPicker lists a reply's code blocks, then all of them and the whole
// reply; each row runs a /copy form, and 1-9 pick a block at once.
func copyPicker(blocks []codeBlock) Overlay {
	var items []any
	for i, b := range blocks {
		item := map[string]any{"text": fmt.Sprintf("%d  %s", i+1, cmp.Or(b.lang, "text")), "run": fmt.Sprintf("copy code %d", i+1)}
		if i < 9 {
			item["hotkey"] = strconv.Itoa(i + 1)
		}
		n := strings.Count(b.text, "\n")
		if b.text != "" && !strings.HasSuffix(b.text, "\n") {
			n++
		}
		var desc strings.Builder
		fmt.Fprintf(&desc, "%d %s", n, plural(n, "line", "lines"))
		for l := range strings.Lines(b.text) {
			if first := strings.TrimSpace(l); first != "" {
				desc.WriteString(" · " + first)
				break
			}
		}
		item["desc"] = desc.String()
		items = append(items, item)
	}
	if len(blocks) > 1 {
		items = append(items, map[string]any{"text": "All code blocks", "run": "copy code"})
	}
	items = append(items, map[string]any{"text": "Whole reply", "run": "copy reply"})
	return Overlay{ID: "copy", Title: "Copy", Origin: lua.OriginBuiltin, Items: items, Verb: "copy"}
}

type codeBlock struct{ lang, text string }

// codeBlocks returns the fenced code blocks of markdown (``` or ~~~, closed
// by a fence of the same character at least as long) with the fence lines
// cut and the opening fence's indentation taken off each line. Unlike
// CommonMark a fence may be indented any amount, since models nest them in
// list items; an unclosed block runs to the end.
func codeBlocks(md string) []codeBlock {
	var out []codeBlock
	var body strings.Builder
	open, indent := "", 0
	for line := range strings.Lines(md) {
		trimmed := strings.TrimLeft(line, " \t")
		bare := strings.TrimRight(trimmed, " \t\r\n")
		run := "" // the fence characters starting the line
		if strings.HasPrefix(bare, "```") || strings.HasPrefix(bare, "~~~") {
			run = bare[:len(bare)-len(strings.TrimLeft(bare, bare[:1]))]
		}
		switch {
		case open == "" && run != "":
			info := bare[len(run):]
			if run[0] == '`' && strings.Contains(info, "`") {
				continue // inline code such as ```x```
			}
			lang, _, _ := strings.Cut(strings.Join(strings.Fields(info), " "), " ")
			out = append(out, codeBlock{lang: lang})
			open, indent = run, len(line)-len(strings.TrimLeft(line, " "))
			body.Reset()
		case open == "":
		case run == bare && run != "" && run[0] == open[0] && len(run) >= len(open):
			out[len(out)-1].text, open = body.String(), ""
		default:
			cut := min(indent, len(line)-len(strings.TrimLeft(line, " ")))
			body.WriteString(line[cut:])
		}
	}
	if open != "" {
		out[len(out)-1].text = body.String()
	}
	return out
}

// ExportSession writes the live session as markdown to path ("" picks
// zemi-<id>.md in the project), says where, and returns the path written.
func (h *Host) ExportSession(path string) (string, error) {
	sid := h.AgentSessionID()
	if sid == "" || h.DB == nil {
		return "", Usagef("no session to export")
	}
	sess, err := h.DB.GetSession(sid)
	if err != nil {
		return "", err
	}
	rows, err := h.DB.Messages(sid)
	if err != nil {
		return "", err
	}
	if path == "" {
		path = filepath.Join(h.Dirs.ProjectDir, "zemi-"+sid[:min(8, len(sid))]+".md")
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(h.Dirs.ProjectDir, path)
	}
	// the user names this path, so it may leave the project; a secret-shaped
	// target is still refused, and only user or builtin code reaches here
	// (zemi.command.run gates Go commands by origin)
	if h.secretPath(path) {
		return "", Usagef("refusing to write over %s", path)
	}
	title := sanitize.String(sess.Title)
	if title == "" {
		title = "zemi session"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n_zemi session %s, %s_\n", title, sid, time.UnixMilli(sess.CreatedAt).Format(time.DateOnly+" "+ClockLayout))
	_, tr := historyFrom(rows)
	exportMarkdown(&b, tr)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	h.Notify(logging.Info, "Session exported", "saved to "+TildePath(h.relPath(path)))
	return path, nil
}

// exportMarkdown writes transcript rows as markdown, one header over each run
// of rows by one speaker. Every string is sanitized: a file the user later
// cats must not carry escapes, and historyFrom leaves message text raw.
func exportMarkdown(b *strings.Builder, tr []ChatMessage) {
	speaker, list := "", false // list: the last line written is a tool row's
	for _, m := range tr {
		who := "zemi"
		if m.Role == fantasy.MessageRoleUser {
			who = "You"
		}
		if who != speaker || who == "You" {
			fmt.Fprintf(b, "\n## %s\n", who)
			speaker, list = who, false
		}
		text := strings.TrimSpace(sanitize.String(m.Text))
		switch m.Kind {
		case RowReasoning:
			continue
		case RowTool:
			if !list {
				b.WriteString("\n")
			}
			fmt.Fprintf(b, "- `%s` %s%s\n", sanitize.String(m.Tool), sanitize.String(clipRunes(strings.Join(strings.Fields(cmp.Or(m.Label, m.Text)), " "), 120)), callEnd(m.Status))
		case RowNotice, RowCompact:
			fmt.Fprintf(b, "\n_%s_\n", text)
		default:
			fmt.Fprintf(b, "\n%s\n", text)
		}
		list = m.Kind == RowTool
	}
}

// callEnd names a tool call that did not finish well, after its row.
func callEnd(s CallStatus) string {
	switch s {
	case CallErr:
		return " (failed)"
	case CallStopped:
		return " (stopped)"
	default:
		return ""
	}
}

// ForkSession copies the live session's messages into a new session and
// switches to it, saying so; it returns the new id.
func (h *Host) ForkSession() (string, error) {
	sid := h.AgentSessionID()
	if sid == "" || h.DB == nil {
		return "", Usagef("no session to fork")
	}
	if h.AgentActive() {
		return "", Usagef("a turn is running; esc twice interrupts it")
	}
	if n := h.tasks.runningFor(sid); n > 0 {
		return "", Usagef("%d background %s still running; stop them from /agents first", n, plural(n, "agent", "agents"))
	}
	sess, err := h.DB.GetSession(sid)
	if err != nil {
		return "", err
	}
	id := store.NewID()
	if err := h.DB.ForkSession(sess, id, strings.TrimSuffix(sess.Title, " (fork)")+" (fork)"); err != nil {
		return "", err
	}
	// the copied history is the parent's to reflect on when it closes; the
	// fork's delta is only what it adds, so a rejected memory is not proposed twice
	if n, err := h.DB.MessageCount(id); err == nil {
		h.reflectSeen(id, n)
	}
	if err := h.ResumeSession(id); err != nil {
		return "", err
	}
	h.Notify(logging.Info, "Session forked", "the original stays in /sessions")
	return id, nil
}

// todoSnapshot is the live session's todo list for widgets, reloaded only
// after the todos tool wrote or the session changed.
func (h *Host) todoSnapshot() []store.Todo {
	sid := h.AgentSessionID()
	h.todoMu.Lock()
	defer h.todoMu.Unlock()
	if h.todoSID != sid || h.todosStale.Swap(false) {
		h.todoSID, h.todos = sid, nil
		if sid != "" && h.DB != nil {
			h.todos, _ = h.DB.Todos(sid)
		}
	}
	return h.todos
}

// OpenUsage shows how full the context is, what the fixed prompt is made of,
// and token use and cost: the live session, then today, the last 7 days and
// all time across this project's sessions.
func (h *Host) OpenUsage() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	if h.DB == nil {
		return errors.New("usage needs the session store")
	}
	s := h.AgentStream()
	fmtTok := func(n int64) string {
		switch {
		case n >= 1_000_000:
			return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e6), ".0") + "M"
		case n >= 1000:
			return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e3), ".0") + "k"
		}
		return strconv.FormatInt(n, 10)
	}
	var rows [][]Segment
	text := func(hl, t string) Segment { return Segment{Text: t, HL: hl} }
	section := func(title, note string) {
		if len(rows) > 0 {
			rows = append(rows, []Segment{})
		}
		rows = append(rows, []Segment{text("Host:Title", title), text("Host:Hint", note)})
	}
	// bar is a meter of w cells, part/whole of it filled
	bar := func(part, whole int64, w int, hl string) []Segment {
		n := 0
		if whole > 0 {
			n = int(min(max(part*int64(w)/whole, min(part, 1)), int64(w))) // anything at all gets a cell
		}
		return []Segment{{Text: strings.Repeat("█", n), HL: hl, NoCopy: true}, {Text: strings.Repeat("░", w-n) + "  ", HL: "Host:Rule", NoCopy: true}}
	}
	if s.CtxWindow > 0 && s.LastIn > 0 { // a resumed session has no estimate before its first turn
		pct := s.LastIn * 100 / s.CtxWindow
		hl := "Host:HintKey"
		if pct >= 90 {
			hl = "Host:Danger"
		} else if pct >= 70 {
			hl = "Host:Warn"
		}
		section("Context", "")
		rows = append(rows, append(append([]Segment{text("", "  ")}, bar(s.LastIn, s.CtxWindow, 32, hl)...),
			text("Host:Value", fmtTok(s.LastIn)), text("Host:Hint", fmt.Sprintf(" of %s · %d%%", fmtTok(s.CtxWindow), pct))))
	}
	if parts, total := h.promptBreakdown(); total > 0 {
		section("Prompt", "  "+fmtTok(total)+" sent with every request")
		parts = slices.Clone(parts)
		slices.SortStableFunc(parts, func(a, b promptPart) int { return cmp.Compare(b.chars, a.chars) })
		sum := 0
		for _, p := range parts {
			sum += p.chars
		}
		for _, p := range parts {
			if p.chars == 0 {
				continue
			}
			row := append([]Segment{text("Host:Value", fmt.Sprintf("  %-14s", p.name))}, bar(int64(p.chars), int64(sum), 16, "Host:HintKey")...)
			rows = append(rows, append(row, text("Host:Value", fmt.Sprintf("%6s", fmtTok(int64(p.chars/4)))),
				text("Host:Hint", fmt.Sprintf("  %3d%%", p.chars*100/sum))))
		}
	}
	type window struct {
		label string
		u     store.Usage
	}
	windows := []window{{"this session", store.Usage{Prompt: s.SessionIn, Completion: s.SessionOut, Cost: s.SessionCost}}}
	y, m, d := time.Now().Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	for _, w := range []struct {
		label string
		since time.Time
	}{{"today", midnight}, {"7 days", midnight.AddDate(0, 0, -6)}, {"all time", time.Time{}}} {
		ms := int64(0)
		if !w.since.IsZero() {
			ms = w.since.UnixMilli()
		}
		u, err := h.DB.UsageSince(ms)
		if err != nil {
			return err
		}
		windows = append(windows, window{w.label, u})
	}
	// a model with no prices costs $0.00 on every row: one line says so
	// instead of a column of zeros
	id := h.sessionModelID()
	spec, _ := h.modelSpec(id)
	priced := spec.CostPer1MIn > 0 || spec.CostPer1MOut > 0 || s.SessionCost > 0 || windows[len(windows)-1].u.Cost > 0
	head := fmt.Sprintf("%18s%9s%10s", "in", "out", "sessions") // over the rows' columns, past the 6 cells of the title
	if priced {
		head += "      cost"
	}
	section("Tokens", head)
	for i, w := range windows {
		count := ""
		if w.u.Sessions > 0 {
			count = strconv.Itoa(w.u.Sessions)
		}
		row := []Segment{text("Host:Value", fmt.Sprintf("  %-14s%8s%9s", w.label, fmtTok(w.u.Prompt), fmtTok(w.u.Completion))), text("Host:Hint", fmt.Sprintf("%10s", count))}
		if priced {
			row = append(row, text("Host:Value", fmt.Sprintf("%10s", fmt.Sprintf("$%.2f", w.u.Cost))))
		}
		// a broken cache prefix (a per-turn system section, a model switch) has
		// no other symptom than the bill
		if i == 0 && s.CacheBilled > 0 { // the live session
			row = append(row, text("Host:Hint", fmt.Sprintf("   %d%% cached", s.CacheRead*100/s.CacheBilled)))
		}
		rows = append(rows, row)
	}
	if id != "" && !priced {
		rows = append(rows, []Segment{}, []Segment{text("Host:Hint", "No cost: "+id+" has no prices. Set cost_per_1m_in and cost_per_1m_out on the model.")})
	}
	var plain strings.Builder
	for _, r := range rows {
		for _, seg := range r {
			if !seg.NoCopy {
				plain.WriteString(seg.Text)
			}
		}
		plain.WriteByte('\n')
	}
	ui.OpenOverlay(Overlay{ID: "usage", Title: "Usage", Origin: lua.OriginBuiltin, Body: plain.String(), BodySegs: rows, Anchor: OverlayBottomRight})
	return nil
}
