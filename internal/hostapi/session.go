// session.go is the session lifecycle (DESIGN §6.3, §10.3): /new closes the
// live session and starts clean, /sessions lists recent ones, /resume
// rebuilds the model history and the transcript from the store. Closing a
// session is the reflector's session-close trigger.
package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/sanitize"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
)

// NewSession closes the live session; the next send creates a fresh one.
func (h *Host) NewSession() error {
	if _, err := h.closeSession(); err != nil {
		return err
	}
	h.setTranscript("zemi", nil)
	return nil
}

// ResumeSession reopens a stored session ("" means the most recent one).
func (h *Host) ResumeSession(id string) error {
	if h.DB == nil {
		return errors.New("session store unavailable")
	}
	if id == "" {
		list, err := h.DB.ListSessions(1)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			return errors.New("no sessions to resume")
		}
		id = list[0].ID
	}
	if id == h.AgentSessionID() {
		return nil
	}
	sess, err := h.DB.GetSession(id)
	if err != nil {
		return fmt.Errorf("session %s: %w", id, err)
	}
	rows, err := h.DB.Messages(id)
	if err != nil {
		return err
	}
	if _, err := h.closeSession(); err != nil {
		return err
	}
	history, tr := resumeHistory(rows)
	ag := h.agents
	ag.mu.Lock()
	ag.sessionID, ag.titled = id, true
	// A bare runner carries the history; AgentSend copies it into the
	// runner it builds for the next turn.
	ag.runner = &agent.Runner{History: history}
	ag.sessIn, ag.sessOut, ag.sessCost = sess.PromptTokens, sess.CompletionTokens, sess.Cost
	ag.digest = h.digestFor()
	ag.env = ""
	ag.mu.Unlock()
	h.setTranscript(sess.Title, tr)
	h.EmitAsync("session.open", map[string]any{"id": id, "title": sess.Title})
	return nil
}

// OpenSessions lists recent sessions; enter on a row resumes it.
func (h *Host) OpenSessions() error {
	ui := h.hooks()
	if h.DB == nil || ui == nil {
		return errors.New("session store unavailable")
	}
	list, err := h.DB.ListSessions(50)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return errors.New("no sessions yet")
	}
	current := h.AgentSessionID()
	previews, _ := h.DB.SessionPreviews() // best effort: a bare list still resumes
	items := make([]any, 0, len(list))
	for _, s := range list {
		mark, title := "  ", s.Title
		if s.ID == current {
			mark = "● "
		}
		if title == "" {
			title = "(untitled)"
		}
		title = clipRunes(title, 32)
		title += strings.Repeat(" ", max(0, 32-len([]rune(title))))
		p := previews[s.ID]
		var tail []string
		if p.Count > 0 {
			tail = append(tail, fmt.Sprintf("%d %s", p.Count, plural(p.Count, "message", "messages")))
		}
		if line := firstUserLine(p.FirstJSON); line != "" {
			tail = append(tail, line)
		}
		items = append(items, map[string]any{
			"text": strings.TrimRight(fmt.Sprintf("%s%-7s  %s  %s", mark, ago(s.UpdatedAt), title, strings.Join(tail, " · ")), " "),
			"run":  "resume " + s.ID,
		})
	}
	ui.OpenOverlay(Overlay{ID: "sessions", Title: "Sessions", Origin: lua.OriginBuiltin, Items: items})
	return nil
}

// firstUserLine is the first typed line of a stored user message, for the
// session picker; attachments and later lines stay out.
func firstUserLine(partsJSON string) string {
	var m fantasy.Message
	if partsJSON == "" || json.Unmarshal([]byte(partsJSON), &m) != nil {
		return ""
	}
	var text strings.Builder
	for _, part := range m.Content {
		if p, ok := part.(fantasy.TextPart); ok {
			text.WriteString(p.Text)
		}
	}
	s, _, _ := strings.Cut(text.String(), attachMarker)
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return truncRunes(sanitize.String(s), 60)
}

// closeSession detaches the live session (refused mid-turn) and fires its
// close trigger; it returns the closed id.
func (h *Host) closeSession() (string, error) {
	ag := h.agents
	ag.mu.Lock()
	if ag.active {
		ag.mu.Unlock()
		return "", errors.New("a turn is running; esc interrupts it")
	}
	prev := ag.sessionID
	h.recentStale.Store(true)
	ag.sessionID, ag.runner, ag.steering = "", nil, nil
	ag.textBuf, ag.reasonBuf, ag.tools = "", "", nil
	ag.sessIn, ag.sessOut, ag.lastIn, ag.sessCost = 0, 0, 0, 0
	ag.compactFails, ag.compacted = 0, false
	ag.digest, ag.memAuto, ag.cfgAuto = "", 0, 0
	ag.mu.Unlock()
	if prev != "" {
		h.EmitAsync("session.close", map[string]any{"id": prev})
		h.reflectOnClose(prev)
	}
	return prev, nil
}

func (h *Host) setTranscript(title string, tr []ChatMessage) {
	h.tMu.Lock()
	h.session.Title, h.session.Transcript, h.view = title, tr, ""
	h.tMu.Unlock()
	h.refresh()
}

// resumeHistory rebuilds model history and transcript from stored rows; a
// compacted session's model history starts at its last summary. Scanning
// rows instead of trusting sessions.summary_message_id keeps a rewind that
// cut the summary from resuming a summary it no longer has.
func resumeHistory(rows []*store.Message) ([]fantasy.Message, []ChatMessage) {
	history, tr := historyFrom(rows)
	for i := len(rows) - 1; i >= 0; i-- {
		var m fantasy.Message
		if rows[i].Role != "user" || json.Unmarshal([]byte(rows[i].PartsJSON), &m) != nil {
			continue
		}
		if _, ok := summaryOf(m); ok {
			history, _ = historyFrom(rows[i:])
			break
		}
	}
	return history, tr
}

// historyFrom decodes stored messages into model history and transcript
// rows. Rows that are not fantasy messages (tool telemetry) are skipped.
func historyFrom(rows []*store.Message) ([]fantasy.Message, []ChatMessage) {
	var history []fantasy.Message
	var tr []ChatMessage
	toolRow := map[string]int{} // tool call id -> transcript index
	for _, row := range rows {
		var m fantasy.Message
		if json.Unmarshal([]byte(row.PartsJSON), &m) != nil || len(m.Content) == 0 {
			continue
		}
		history = append(history, m)
		if row.Role == compactTailRole {
			continue // the transcript already has the original
		}
		var text strings.Builder
		for _, part := range m.Content {
			switch p := part.(type) {
			case fantasy.TextPart:
				text.WriteString(p.Text)
			case fantasy.ReasoningPart:
				tr = append(tr, ChatMessage{Role: "assistant", Kind: "reasoning", Text: p.Text})
			case fantasy.ToolCallPart:
				toolRow[p.ToolCallID] = len(tr)
				tr = append(tr, ChatMessage{Role: "assistant", Kind: "tool", Tool: p.ToolName, Text: toolSummary(p.Input), Label: toolLabel(p.Input), Status: "ok"})
			case fantasy.ToolResultPart:
				if i, ok := toolRow[p.ToolCallID]; ok && p.Output != nil {
					tr[i].Output = outputTail(agent.ToolOutput(p.Output))
					if p.Output.GetType() == fantasy.ToolResultContentTypeError {
						tr[i].Status = "err"
					}
				}
			}
		}
		s := text.String()
		if m.Role == fantasy.MessageRoleUser {
			if _, ok := summaryOf(m); ok {
				tr = append(tr, ChatMessage{Role: "assistant", Kind: "compact", Text: "compacted earlier messages"})
				continue
			}
			s, _, _ = strings.Cut(s, attachMarker) // show what was typed
		}
		if s = strings.TrimSpace(s); s != "" {
			role := "assistant"
			if m.Role == fantasy.MessageRoleUser {
				role = "user"
			}
			tr = append(tr, ChatMessage{Role: role, Text: s})
		}
	}
	return history, tr
}

const titleSystem = `Name a coding conversation for a session list. Reply with the title only: 3 to 6 words, no quotes, no trailing period, in the language of the user's message.`

// autoTitle names a new session from its first exchange with a short model
// call in the background; any failure keeps the first-line title.
func (h *Host) autoTitle(sid, user, reply string) {
	if v, ok := h.Settings.Get("session.auto_title"); ok && v == false {
		return
	}
	if h.DB == nil {
		return
	}
	h.GoSafe("session title", func() {
		model, err := h.modelFor("agent.small_model")
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := generateText(ctx, model, titleSystem, "User: "+truncRunes(user, 2000)+"\n\nAssistant: "+truncRunes(reply, 2000))
		if err != nil {
			h.Log.Debug("session", "title generation failed", map[string]any{"error": err.Error()})
			return
		}
		title := sanitize.String(clipRunes(strings.Trim(strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]), `"'.# `), 60))
		if title == "" {
			return
		}
		if err := h.DB.RenameSession(sid, title); err != nil {
			return
		}
		h.recentStale.Store(true)
		if h.AgentSessionID() == sid {
			h.tMu.Lock()
			h.session.Title = title
			h.tMu.Unlock()
			h.refresh()
		}
	})
}

// recentSessions feeds the splash screen. Widget renders must not wait on
// the store, so the list is cached and reloaded only after a session
// starts, closes or resumes.
func (h *Host) recentSessions() []*store.Session {
	h.recentMu.Lock()
	defer h.recentMu.Unlock()
	if h.recentStale.Swap(false) && h.DB != nil {
		h.recent, _ = h.DB.ListSessions(3)
	}
	return h.recent
}

func ago(ms int64) string {
	d := time.Since(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
