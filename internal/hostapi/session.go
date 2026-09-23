// session.go is the session lifecycle (DESIGN §6.3, §10.3): /new closes the
// live session and starts clean, /sessions lists recent ones and deletes one, /resume
// rebuilds the model history and the transcript from the store. Closing a
// session is the reflector's session-close trigger.
package hostapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
)

// NewSession closes the live session; the next send creates a fresh one.
func (h *Host) NewSession() error {
	if err := h.closeSession(nil); err != nil {
		return err
	}
	h.setTranscript("zemi", nil)
	return nil
}

// ResumeSession reopens a stored session ("" means the most recent other one).
func (h *Host) ResumeSession(id string) error {
	if h.DB == nil {
		return errors.New("session store unavailable")
	}
	live := h.AgentSessionID()
	if id == "" {
		list, err := h.DB.ListSessions(2)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(list, func(s *store.Session) bool { return s.ID != live })
		if i < 0 {
			h.Notify("warn", "Nothing to resume", "no other session in this project yet")
			return nil
		}
		id = list[i].ID
	}
	if id == live {
		return nil
	}
	sess, err := h.DB.GetSession(id)
	if errors.Is(err, store.ErrNotFound) {
		if h.hooks() == nil {
			return Usagef("no session %q", id)
		}
		return Usagef("no session %q; /sessions lists them", id)
	}
	if err != nil {
		return fmt.Errorf("session %s: %w", id, err)
	}
	rows, err := h.DB.Messages(id)
	if err != nil {
		return err
	}
	history, tr := resumeHistory(rows)
	first := ""
	for _, m := range tr {
		if m.Role == "user" && m.Kind == "" {
			first = m.Text
			break
		}
	}
	digest := h.digestFor()
	ag := h.agents
	err = h.closeSession(func() {
		ag.sessionID, ag.titled, ag.firstUser = id, true, first
		// A bare runner carries the history; AgentSend copies it into the
		// runner it builds for the next turn.
		ag.runner = &agent.Runner{History: history}
		ag.sessIn, ag.sessOut, ag.sessCost = sess.PromptTokens, sess.CompletionTokens, sess.Cost
		// lastIn is the current-context estimate; seed it from the history and
		// the prompt so the context readout and compactDue see the window it
		// had until the next step reports a real count.
		ag.lastIn = estTokens(history...) + ag.promptTokens
		ag.digest, ag.env = digest, ""
	})
	if err != nil {
		return err
	}
	h.seedReads(resumeRows(rows))
	_, loaded := h.sessionLogs()
	seedLoaded(loaded, history)
	h.setTranscript(sess.Title, tr)
	h.EmitAsync("session.open", map[string]any{"id": id, "title": sess.Title})
	return nil
}

// OpenSessions lists recent sessions; enter on a row resumes it.
func (h *Host) OpenSessions() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	if h.DB == nil {
		return errors.New("session store unavailable")
	}
	list, err := h.DB.ListSessions(50)
	if err != nil {
		return err
	}
	current := h.AgentSessionID()
	ids := make([]string, 0, len(list))
	for _, s := range list {
		ids = append(ids, s.ID)
	}
	previews, _ := h.DB.SessionPreviews(ids) // best effort: a bare list still resumes
	items := make([]any, 0, len(list))
	for _, s := range list {
		p := previews[s.ID]
		desc := []string{ago(s.UpdatedAt)}
		if p.Count > 0 {
			desc = append(desc, fmt.Sprintf("%d %s", p.Count, plural(p.Count, "message", "messages")))
		}
		if line := firstUserLine(p.FirstJSON); line != "" && !strings.EqualFold(line, s.Title) {
			desc = append(desc, line)
		}
		item := map[string]any{"text": cmp.Or(s.Title, "Untitled"), "desc": strings.Join(desc, " · ")}
		if s.ID == current {
			item["key"] = "current"
		} else {
			item["detail"] = strings.Join(desc, " · ") + "\n\nDelete removes the conversation, its subagent runs and its scratchpad for good. Memories learned from it stay."
			item["actions"] = []any{
				map[string]any{"text": "Resume", "hotkey": "r", "run": "resume " + s.ID},
				map[string]any{"text": "Delete", "hotkey": "x", "hl": "Danger", "run": "sessions delete " + s.ID},
			}
		}
		items = append(items, item)
	}
	ui.OpenOverlay(Overlay{ID: "sessions", Title: "Sessions", Origin: lua.OriginBuiltin, Items: items, Empty: "No sessions in this project yet."})
	return nil
}

// DeleteSession removes a stored session other than the live one, with its
// subagent runs and scratchpad. The rewrite that clears the text from disk
// takes a while on a large store, so it runs off the caller's goroutine.
func (h *Host) DeleteSession(id string) error {
	if h.DB == nil {
		return errors.New("session store unavailable")
	}
	if id == "" || id == h.AgentSessionID() {
		return Usagef("the current session cannot be deleted; /new first")
	}
	for _, run := range h.tasks.list(id) {
		if run.isRunning() {
			return Usagef("an agent from that session is still running; /agents stops it")
		}
	}
	h.GoSafe("delete session", func() {
		if err := h.DB.DeleteSession(id); err != nil {
			h.Notify("error", "Could not delete the session", err.Error())
			return
		}
		h.recentStale.Store(true)
		if h.Dirs.StateHome != "" {
			_ = os.RemoveAll(h.scratchFor(id))
			_ = os.RemoveAll(filepath.Join(h.Dirs.StateHome, "sandbox-tmp", id))
		}
		h.Notify("info", "Session deleted", "")
	})
	return nil
}

// InputHistory returns the last limit messages the user typed in this
// project, oldest first, for the composer history. Every session counts:
// the prompt typed before /new is still the one worth recalling.
func (h *Host) InputHistory(limit int) []string {
	if h.DB == nil || limit <= 0 {
		return nil
	}
	rows, err := h.DB.UserMessages(limit)
	if err != nil {
		return nil
	}
	slices.Reverse(rows)
	var out []string
	for _, row := range rows {
		if text, ok := userText(row); ok {
			out = append(out, text)
		}
	}
	return out
}

// firstUserLine is the first typed line of a stored user message, for the
// session picker; attachments and later lines stay out.
func firstUserLine(partsJSON string) string {
	text, _ := userText(&store.Message{Role: "user", PartsJSON: partsJSON})
	line, _, _ := strings.Cut(text, "\n")
	return truncRunes(sanitize.String(line), 60)
}

// closeSession detaches the live session (refused mid-turn) and fires its
// close trigger. open runs under the same lock, so the next session is in
// place before a send (a background agent's report) can start a fresh one.
func (h *Host) closeSession(open func()) error {
	ag := h.agents
	ag.mu.Lock()
	if ag.active {
		ag.mu.Unlock()
		return errors.New("a turn is running; esc twice interrupts it")
	}
	prev, reports := ag.sessionID, ag.steering
	h.recentStale.Store(true)
	// turnMsgID too: a write arriving late would checkpoint against a
	// message of the session just closed
	ag.sessionID, ag.runner, ag.steering, ag.turnMsgID = "", nil, nil, ""
	ag.textBuf, ag.reasonBuf, ag.tools = "", "", nil
	ag.sessIn, ag.sessOut, ag.lastIn, ag.sessCost = 0, 0, 0, 0
	ag.cacheRead, ag.cacheBilled = 0, 0
	ag.compactFails, ag.compacted = 0, false
	ag.digest, ag.memAuto, ag.promptText = "", 0, ""
	ag.todoInProgress, ag.todoSteps, ag.todoPending, ag.todoNag = "", 0, false, ""
	if open != nil {
		open()
	}
	ag.mu.Unlock()
	// only background reports wait at idle: the user hears of them, as of
	// one that arrives after the session moved on
	h.notifyReports(reports)
	h.refreshPending()
	if prev != "" {
		h.EmitAsync("session.close", map[string]any{"id": prev})
		h.reflectOnClose(prev)
	}
	return nil
}

// setTranscript replaces the chat (new, resume, rewind), which opens at its
// tail: a place scrolled to in the old one means nothing in the new.
func (h *Host) setTranscript(title string, tr []ChatMessage) {
	h.tMu.Lock()
	h.session.Title, h.session.Transcript, h.view = title, tr, ""
	h.trEpoch.Add(1)
	h.tMu.Unlock()
	h.scroll("bottom", "")
	h.refresh()
}

// resumeHistory rebuilds model history and transcript from stored rows; a
// compacted session's model history starts at its last summary row, so a
// rewind that cut the summary resumes from the one before it.
func resumeHistory(rows []*store.Message) ([]fantasy.Message, []ChatMessage) {
	_, tr := historyFrom(rows)
	history, _ := historyFrom(resumeRows(rows))
	return history, tr
}

// resumeRows are the rows the resumed model history covers: everything from
// the last compaction summary on, so a reader sees what the model holds.
func resumeRows(rows []*store.Message) []*store.Message {
	for i, row := range slices.Backward(rows) {
		var m fantasy.Message
		if row.Role != "user" || json.Unmarshal([]byte(row.PartsJSON), &m) != nil {
			continue
		}
		if _, ok := summaryOf(m); ok {
			return rows[i:]
		}
	}
	return rows
}

// historyFrom decodes stored messages into model history and transcript
// rows, the rows the live turn drew. Rows that are not fantasy messages
// are tool telemetry: a call's diff, run and summary, a step's thought time.
func historyFrom(rows []*store.Message) ([]fantasy.Message, []ChatMessage) {
	var history []fantasy.Message
	var tr []ChatMessage
	toolRow := map[string]int{} // tool call id -> transcript index
	type telemetry struct {
		ID, Diff, Run, Detail string
		ThoughtMS             int64 `json:"thought_ms"`
	}
	calls := map[string]telemetry{}
	for _, row := range rows {
		var tc telemetry
		if row.Role == "tool" && json.Unmarshal([]byte(row.PartsJSON), &tc) == nil && tc.ID != "" {
			calls[tc.ID] = tc
		}
	}
	reasoning := -1 // the transcript index of the latest reasoning row
	for _, row := range rows {
		var tc telemetry
		if row.Role == "tool" && json.Unmarshal([]byte(row.PartsJSON), &tc) == nil && tc.ThoughtMS > 0 && reasoning >= 0 {
			tr[reasoning].Stat.ThoughtMS += tc.ThoughtMS
			continue
		}
		var m fantasy.Message
		if json.Unmarshal([]byte(row.PartsJSON), &m) != nil || len(m.Content) == 0 {
			continue
		}
		history = append(history, m)
		if row.Role == compactTailRole {
			continue // the transcript already has the original
		}
		var text strings.Builder
		// text streamed before a call is a row of its own above it, as the
		// live turn flushed it; reasoning never cuts a reply (ReasoningDelta)
		reply := func() {
			if s := strings.TrimSpace(text.String()); s != "" {
				tr = append(tr, ChatMessage{Role: "assistant", Text: s})
				text.Reset()
			}
		}
		for _, part := range m.Content {
			switch p := part.(type) {
			case fantasy.TextPart:
				text.WriteString(p.Text)
			case fantasy.ReasoningPart:
				if strings.TrimSpace(p.Text) == "" {
					break
				}
				if reasoning >= 0 && reasoning == len(tr)-1 {
					// the live turn shows reasoning with nothing after it as one block
					tr[reasoning].Text += p.Text
				} else {
					reasoning = len(tr)
					tr = append(tr, ChatMessage{Role: "assistant", Kind: "reasoning", Text: p.Text, Stat: &TurnStat{}})
				}
			case fantasy.ToolCallPart:
				reply()
				toolRow[p.ToolCallID] = len(tr)
				tc := calls[p.ToolCallID]
				tr = append(tr, ChatMessage{Role: "assistant", Kind: "tool", Tool: p.ToolName, Text: toolSummary(p.Input), Label: toolLabel(p.Input), Status: "ok", Diff: tc.Diff, Run: tc.Run, Detail: tc.Detail})
			case fantasy.ToolResultPart:
				if i, ok := toolRow[p.ToolCallID]; ok && p.Output != nil {
					out := agent.ToolOutput(p.Output)
					tr[i].Output = shortOutput(out)
					tr[i].Status = toolStatus(tr[i].Tool, p.Output.GetType() != fantasy.ToolResultContentTypeError, out, false)
				}
			}
		}
		s := text.String()
		if m.Role == fantasy.MessageRoleUser {
			if _, ok := summaryOf(m); ok {
				tr = append(tr, ChatMessage{Role: "assistant", Kind: "compact", Text: "Compacted earlier messages"})
				continue
			}
			var tail string
			s, tail, _ = strings.Cut(s, agent.AttachMarker) // show what was typed
			for _, head := range reportHeads(tail) {
				tr = append(tr, ChatMessage{Role: "assistant", Kind: "notice", Text: head})
			}
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

const titleSystem = `Name the coding conversation below for a session list. Do not answer or continue it. Reply with the title only: 3 to 6 words naming the task, file and identifier names kept exact, no quotes, no trailing period, in the language the user wrote in.`

// autoTitle names a new session from its first exchange with a short model
// call in the background; any failure keeps the first-line title.
func (h *Host) autoTitle(sid, user, reply string) {
	if !h.Settings.Bool("session.auto_title") {
		return
	}
	if h.DB == nil {
		return
	}
	h.GoSafe("session title", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := h.generate(ctx, sid, titleSystem, "<conversation>\nuser: "+truncRunes(user, 2000)+"\n\nassistant: "+truncRunes(reply, 2000)+"\n</conversation>", nil, nil, "models.small")
		if err != nil {
			h.Log.Debug("session", "title generation failed", map[string]any{"error": err.Error()})
			return
		}
		first, _, _ := strings.Cut(strings.TrimSpace(concatText(res.Response.Content)), "\n")
		title := sanitize.String(truncRunes(strings.Trim(strings.TrimSpace(first), `"'.# `), 60))
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
