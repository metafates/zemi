// export.go holds session copies: /export writes the conversation as
// markdown, /fork continues it in a new session so the original stays as it
// was. It also caches the todo list the chat widget shows.
package hostapi

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
)

// CopyReply copies the live session's last reply as the model wrote it (a
// drag copies rendered, wrapped screen rows). args is "" (a picker when the
// reply has code blocks), "reply", "code" (every block) or "code <n>".
func (h *Host) CopyReply(args string) error {
	sid, ui := h.AgentSessionID(), h.hooks()
	if sid == "" || h.DB == nil || ui == nil {
		return errors.New("no reply to copy")
	}
	rows, err := h.DB.Messages(sid)
	if err != nil {
		return err
	}
	history, _ := historyFrom(rows)
	text := sanitize.String(lastAssistantText(history))
	if text == "" {
		return errors.New("no reply to copy")
	}
	blocks := codeBlocks(text)
	f := strings.Fields(args)
	switch {
	case len(f) == 0 && len(blocks) == 0, len(f) == 1 && f[0] == "reply":
		ui.Copy(text)
	case len(f) == 0:
		ui.OpenOverlay(copyPicker(blocks))
	case f[0] != "code" || len(f) > 2:
		return errors.New("usage: /copy [reply | code [n]]")
	case len(blocks) == 0:
		return errors.New("the last reply has no code blocks")
	case len(f) == 1:
		parts := make([]string, len(blocks))
		for i, b := range blocks {
			parts[i] = strings.TrimSuffix(b.text, "\n")
		}
		ui.Copy(strings.Join(parts, "\n\n") + "\n")
	default:
		n, err := strconv.Atoi(f[1])
		if err != nil || n < 1 || n > len(blocks) {
			return fmt.Errorf("no code block %s: the last reply has %d", f[1], len(blocks))
		}
		ui.Copy(blocks[n-1].text)
	}
	return nil
}

// copyPicker lists a reply's code blocks, then all of them and the whole
// reply; each row runs a /copy form, and 1-9 pick a block at once.
func copyPicker(blocks []codeBlock) Overlay {
	var items []any
	var rows [][]Segment
	for i, b := range blocks {
		item := map[string]any{"run": fmt.Sprintf("copy code %d", i+1)}
		if i < 9 {
			item["hotkey"] = strconv.Itoa(i + 1)
		}
		n := strings.Count(b.text, "\n")
		if b.text != "" && !strings.HasSuffix(b.text, "\n") {
			n++
		}
		lines := fmt.Sprintf(" · %d lines · ", n)
		if n == 1 {
			lines = " · 1 line · "
		}
		first := ""
		for l := range strings.Lines(b.text) {
			if first = strings.TrimSpace(l); first != "" {
				break
			}
		}
		items = append(items, item)
		rows = append(rows, []Segment{{Text: strconv.Itoa(i + 1), HL: "Host:HintKey"}, {Text: "  " + cmp.Or(b.lang, "text")}, {Text: lines, HL: "Comment"}, {Text: first}})
	}
	if len(blocks) > 1 {
		items = append(items, map[string]any{"text": "All code blocks", "run": "copy code"})
		rows = append(rows, nil)
	}
	items = append(items, map[string]any{"text": "Whole reply", "run": "copy reply"})
	rows = append(rows, nil)
	return Overlay{ID: "copy", Title: "Copy", Origin: lua.OriginBuiltin, Items: items, ItemText: rows, Verb: "copy"}
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
// zemi-<id>.md in the project) and returns the path written.
func (h *Host) ExportSession(path string) (string, error) {
	sid := h.AgentSessionID()
	if sid == "" || h.DB == nil {
		return "", errors.New("no session to export")
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
		return "", fmt.Errorf("refusing to write over %s", path)
	}
	title := sanitize.String(sess.Title)
	if title == "" {
		title = "zemi session"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n_zemi session %s, %s_\n", title, sid, time.UnixMilli(sess.CreatedAt).Format("2006-01-02 15:04"))
	for _, r := range rows {
		if text, ok := userText(r); ok {
			fmt.Fprintf(&b, "\n## You\n\n%s\n", sanitize.String(text))
			continue
		}
		var m fantasy.Message
		if r.Role != "assistant" || json.Unmarshal([]byte(r.PartsJSON), &m) != nil {
			continue
		}
		var calls []string
		for _, part := range m.Content {
			if tc, ok := part.(fantasy.ToolCallPart); ok {
				calls = append(calls, fmt.Sprintf("- `%s` %s", sanitize.String(tc.ToolName), sanitize.String(truncRunes(strings.Join(strings.Fields(toolSummary(tc.Input)), " "), 120))))
			}
		}
		// a file the user later cats: model text must not carry escapes
		text := strings.TrimSpace(sanitize.String(concatText(m.Content)))
		if text == "" && len(calls) == 0 {
			continue
		}
		b.WriteString("\n## zemi\n\n")
		if text != "" {
			b.WriteString(text + "\n")
		}
		if len(calls) > 0 {
			if text != "" {
				b.WriteString("\n")
			}
			b.WriteString(strings.Join(calls, "\n") + "\n")
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// ForkSession copies the live session's messages into a new session and
// switches to it; it returns the new id.
func (h *Host) ForkSession() (string, error) {
	sid := h.AgentSessionID()
	if sid == "" || h.DB == nil {
		return "", errors.New("no session to fork")
	}
	if h.AgentActive() {
		return "", errors.New("a turn is running; esc interrupts it")
	}
	if n := h.tasks.running(); n > 0 {
		return "", fmt.Errorf("%d background %s still running; stop them from /agents first", n, plural(n, "agent", "agents"))
	}
	sess, err := h.DB.GetSession(sid)
	if err != nil {
		return "", err
	}
	id := store.NewID()
	if err := h.DB.ForkSession(sess, id, strings.TrimSuffix(sess.Title, " (fork)")+" (fork)"); err != nil {
		return "", err
	}
	return id, h.ResumeSession(id)
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

// OpenUsage shows token use and cost: the live session, then today, the
// last 7 days and all time across this project's sessions.
func (h *Host) OpenUsage() error {
	ui := h.hooks()
	if ui == nil || h.DB == nil {
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
		return fmt.Sprint(n)
	}
	// fixed columns stay whole in the narrowest box, a wrapped row would break the table
	row := func(label string, sessions int, in, out int64, cost float64) string {
		count := "        "
		if sessions > 0 {
			count = fmt.Sprintf("%3d sess", sessions)
		}
		return fmt.Sprintf("%-12s %6s in  %6s out  %s  $%.2f", label, fmtTok(in), fmtTok(out), count, cost)
	}
	lines := []string{row("this session", 0, s.SessionIn, s.SessionOut, s.SessionCost)}
	if parts, total := h.promptBreakdown(); total > 0 {
		lines = append(lines, fmt.Sprintf("%-12s %6s every request", "prompt", fmtTok(total)))
		var named []string
		for _, p := range parts {
			if p.chars > 0 {
				named = append(named, fmt.Sprintf("%s %s", p.name, fmtTok(int64(p.chars/4))))
			}
		}
		lines = append(lines, "  "+strings.Join(named, " · "))
	}
	if s.CtxWindow > 0 && s.LastIn > 0 { // a resumed session has no estimate before its first turn
		line := fmt.Sprintf("%-12s %6s of %s tokens", "context", fmtTok(s.LastIn), fmtTok(s.CtxWindow))
		if pct := s.LastIn * 100 / s.CtxWindow; pct > 0 {
			line += fmt.Sprintf(" (%d%%)", pct)
		}
		lines = append(lines, line)
	}
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
		lines = append(lines, row(w.label, u.Sessions, u.Prompt, u.Completion, u.Cost))
	}
	if id := h.sessionModelID(); id != "" {
		if spec, _ := h.modelSpec(id); spec.CostPer1MIn == 0 && spec.CostPer1MOut == 0 {
			lines = append(lines, "", "Cost is $0.00 because "+id+" has no prices: set cost_per_1m_in and cost_per_1m_out on the model.")
		}
	}
	ui.OpenOverlay(Overlay{ID: "usage", Title: "Usage", Origin: lua.OriginBuiltin, Body: strings.Join(lines, "\n")})
	return nil
}
