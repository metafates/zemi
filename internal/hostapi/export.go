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
		path = filepath.Join(h.Dirs.ProjectDir, "zemi-"+sid[:min(8, len(sid))]+".md")
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(h.Dirs.ProjectDir, path)
	}
	title := sess.Title
	if title == "" {
		title = "zemi session"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n_zemi session %s, %s_\n", title, sid, time.UnixMilli(sess.CreatedAt).Format("2006-01-02 15:04"))
	for _, r := range rows {
		if text, ok := userText(r); ok {
			fmt.Fprintf(&b, "\n## You\n\n%s\n", text)
			continue
		}
		var m fantasy.Message
		if r.Role != "assistant" || json.Unmarshal([]byte(r.PartsJSON), &m) != nil {
			continue
		}
		var calls []string
		for _, part := range m.Content {
			if tc, ok := part.(fantasy.ToolCallPart); ok {
				calls = append(calls, fmt.Sprintf("- `%s` %s", tc.ToolName, truncRunes(strings.Join(strings.Fields(toolSummary(tc.Input)), " "), 120)))
			}
		}
		text := strings.TrimSpace(concatText(m.Content))
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
	sess, err := h.DB.GetSession(sid)
	if err != nil {
		return "", err
	}
	rows, err := h.DB.Messages(sid)
	if err != nil {
		return "", err
	}
	id := store.NewID()
	title := strings.TrimSuffix(sess.Title, " (fork)") + " (fork)"
	if err := h.DB.CreateSession(id, "", title, sess.Profile); err != nil {
		return "", err
	}
	summary := ""
	for _, r := range rows {
		copied := &store.Message{SessionID: id, Role: r.Role, PartsJSON: r.PartsJSON, CreatedAt: r.CreatedAt}
		if err := h.DB.AddMessage(copied); err != nil {
			return "", err
		}
		if r.ID == sess.SummaryMessageID {
			summary = copied.ID
		}
	}
	if summary != "" {
		if err := h.DB.SetSummary(id, summary); err != nil {
			return "", err
		}
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
			return fmt.Sprintf("%.1fM", float64(n)/1e6)
		case n >= 1000:
			return fmt.Sprintf("%.1fk", float64(n)/1e3)
		}
		return fmt.Sprint(n)
	}
	row := func(label string, sessions int, in, out int64, cost float64) any {
		text := fmt.Sprintf("%-12s %6s in  %6s out  $%.2f", label, fmtTok(in), fmtTok(out), cost)
		if sessions > 0 {
			text += fmt.Sprintf("  (%d %s)", sessions, plural(sessions, "session", "sessions"))
		}
		return map[string]any{"text": text}
	}
	items := []any{row("this session", 0, s.SessionIn, s.SessionOut, s.SessionCost)}
	if s.CtxWindow > 0 {
		items = append(items, map[string]any{"text": fmt.Sprintf("%-12s %6s of %s tokens (%d%%)", "context", fmtTok(s.LastIn), fmtTok(s.CtxWindow), s.LastIn*100/s.CtxWindow)})
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
		items = append(items, row(w.label, u.Sessions, u.Prompt, u.Completion, u.Cost))
	}
	ui.OpenOverlay(Overlay{ID: "usage", Title: "Usage (cost needs model prices)", Origin: lua.OriginBuiltin, Items: items})
	return nil
}
