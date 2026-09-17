// export.go holds session copies: /export writes the conversation as
// markdown, /fork continues it in a new session so the original stays as it
// was. It also caches the todo list the chat widget shows.
package hostapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
)

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
		path = filepath.Join(h.Dirs.ProjectDir, "zemi-"+shortID(sid)+".md")
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
