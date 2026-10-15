// session.go is the session lifecycle (DESIGN §6.3, §10.3): /new closes the
// live session and starts clean, /sessions lists recent ones and deletes one, /resume
// rebuilds the model history and the transcript from the store. Closing a
// session is the reflector's session-close trigger.

package hostapi

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

var errNoStore = errors.New("session store unavailable")

// errTurnRunning refuses what would cut or leave the conversation mid-turn.
var errTurnRunning = Usagef("a turn is running; esc twice interrupts it")

// NewSession closes the live session; the next send creates a fresh one.
func (h *Host) NewSession() error {
	if err := h.closeSession(nil); err != nil {
		return err
	}
	h.setTranscript("zemi", nil)
	return nil
}

// ResumeCommand is the shell command that reopens session id; the one place
// that names the CLI's resume flag. A run started with -C names its dir, since
// the shell is not there (a /projects switch is such a run); dir is "" for
// none, and the command then runs from the project.
func ResumeCommand(dir, id string) string {
	if dir == "" {
		return "zemi -r " + id
	}
	word := tools.ShellQuote(dir)
	if rest, ok := strings.CutPrefix(perm.TildePath(dir), "~"); ok && tools.ShellQuote(rest) == rest {
		word = "~" + rest // the shell expands it only unquoted
	}
	return "zemi -C " + word + " -r " + id
}

// ResumeSession reopens a stored session ("" means the most recent other one).
func (h *Host) ResumeSession(id string) error {
	if h.DB == nil {
		return errNoStore
	}
	live := h.AgentSessionID()
	if id == "" {
		list, err := h.DB.ListSessions(2)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(list, func(s *store.Session) bool { return s.ID != live })
		if i < 0 {
			h.Notify(slog.LevelWarn, "Nothing to resume yet", "no other session in this project")
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
			return Usagef("no session %q; zemi sessions lists them", id)
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
	h.reconcileBrain(h.partition(store.ScopeProject))
	brain, globs := h.brainFor()
	ag := h.agents
	err = h.closeSession(func() {
		ag.sessionID = id
		// A bare runner carries the history; AgentSend copies it into the
		// runner it builds for the next turn.
		ag.runner, ag.turnModel = &agent.Runner{History: history}, ""
		ag.sessIn, ag.sessOut, ag.sessCost = sess.PromptTokens, sess.CompletionTokens, sess.Cost
		ag.cacheRead, ag.cacheBilled = sess.CacheRead, sess.CacheBilled
		// lastIn is the current-context estimate; seed it from the history and
		// the prompt so the context readout and compactDue see the window it
		// had until the next step reports a real count.
		ag.lastIn = estTokens(history...) + ag.promptTokens
		ag.brain, ag.brainGlobs, ag.env, ag.ctx = brain, globs, "", contextIn(rows)
		ag.goal = goalIn(rows)
	})
	if err != nil {
		return err
	}
	h.seedReads(resumeRows(rows))
	_, loaded := h.sessionLogs()
	seedLoaded(loaded, history)
	h.setTranscript(sess.Title, tr)
	// a list left in progress settles here: nothing runs on a resumed session
	h.settleTodos()
	h.EmitAsync("session.open", map[string]any{"id": id, "title": sess.Title})
	h.captureSessionContext(id, "resume")
	return nil
}

// OpenSessions lists recent sessions; enter on a row resumes it. confirm
// arms the Delete that runs it (Overlay.Confirm).
func (h *Host) OpenSessions(confirm string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	if h.DB == nil {
		return errNoStore
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
		desc, count, prompt := []string{ago(s.UpdatedAt)}, "", firstUserLine(p.FirstJSON)
		if p.Count > 0 {
			count = strconv.Itoa(p.Count)
			desc = append(desc, fmt.Sprintf("%d %s", p.Count, Plural(p.Count, "message", "messages")))
		}
		if strings.EqualFold(prompt, s.Title) {
			prompt = ""
		}
		if prompt != "" {
			desc = append(desc, clipRunes(prompt, 60))
		}
		// a list gives its labels room before descriptions: a whole title would hide the age
		item := map[string]any{"text": SessionMark(s.ID) + " " + cmp.Or(clipRunes(s.Title, 60), "(untitled)"), "desc": strings.Join(desc, " · ")}
		if s.ID == current {
			item["key"] = "current"
			item["detail"] = "zemi runs this session: /new leaves it, and then it can be resumed or deleted here."
		} else {
			item["fields"] = fieldList("updated", ago(s.UpdatedAt), "messages", count, "prompt", prompt)
			item["detail"] = "Delete removes the conversation, its subagent runs and its scratchpad for good. Memories learned from it stay."
			item["actions"] = []any{
				map[string]any{"text": "Resume", "hotkey": "r", "run": "resume " + s.ID},
				map[string]any{"text": "Rename", "hotkey": "n", "run": "sessions rename " + s.ID + " "},
				map[string]any{"text": "Delete", "hotkey": "x", "danger": true, "run": "sessions delete " + s.ID + " --yes"},
			}
		}
		items = append(items, item)
	}
	ui.OpenOverlay(Overlay{
		ID: "sessions", Title: "Sessions", Origin: lua.OriginBuiltin, Items: items, Confirm: confirm, Empty: "No sessions in this project yet. Send a message to start one.",
		Keys: []OverlayKey{{Key: "r", Desc: "resume"}, {Key: "n", Desc: "rename"}, {Key: "x", Desc: "delete"}},
	})
	return nil
}

// DeleteSession removes a stored session other than the live one, with its
// subagent runs and scratchpad, once yes confirms it (AskFirst). The rewrite
// that clears the text from disk takes a while on a large store, so with a UI
// it runs off the caller's goroutine; headless it runs inline, since the
// process exits when the command returns.
func (h *Host) DeleteSession(id string, yes bool) error {
	if h.DB == nil {
		return errNoStore
	}
	if id == "" || id == h.AgentSessionID() {
		return Usagef("the current session cannot be deleted; /new first")
	}
	if err := h.tasks.stillRunning(func(p string) bool { return p == id }); err != nil {
		return err
	}
	s, err := h.DB.GetSession(id)
	if errors.Is(err, store.ErrNotFound) {
		return Usagef("no session %q in this project", id)
	} else if err != nil {
		return err
	}
	if !yes {
		return AskFirst("sessions delete "+id+" --yes", "", h.OpenSessions)
	}
	del := func() error {
		if err := h.DB.DeleteSession(id); err != nil {
			return err
		}
		h.recentStale.Store(true)
		h.removeSessionFiles(id)
		h.Notify(slog.LevelInfo, "Session deleted", s.Title)
		return nil
	}
	if _, err := h.Screen(); err != nil {
		return del()
	}
	h.GoSafe("delete session", func() {
		if err := del(); err != nil {
			h.Notify(slog.LevelError, "Could not delete the session", err.Error())
		}
	})
	return nil
}

// removeSessionFiles removes what a session keeps outside its store: its
// scratchpad and its approved plan.
func (h *Host) removeSessionFiles(sid string) {
	// "" when there is no state or data dir, which RemoveAll ignores
	_ = os.RemoveAll(h.scratchFor(sid))
	_ = os.RemoveAll(h.planPath(sid))
}

// planPath is where a session's approved plan is saved, "" with no session
// or data dir.
func (h *Host) planPath(sid string) string {
	if sid == "" || h.Dirs.DataHome == "" {
		return ""
	}
	return filepath.Join(h.Dirs.DataHome, "plans", sid+".md")
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
	text, _ := userText(&store.Message{Role: string(fantasy.MessageRoleUser), PartsJSON: partsJSON})
	line, _, _ := strings.Cut(text, "\n")
	return sanitize.String(line)
}

// closeSession detaches the live session (refused mid-turn) and fires its
// close trigger. open runs under the same lock, so the next session is in
// place before a send (a background agent's report) can start a fresh one.
func (h *Host) closeSession(open func()) error {
	ag := h.agents
	ag.mu.Lock()
	if ag.active {
		ag.mu.Unlock()
		return errTurnRunning
	}
	prev, reports := ag.sessionID, ag.steering
	h.swap.Store(true)
	h.recentStale.Store(true)
	h.instrStale.Store(true) // the session may have written instructions
	// turnMsgID too: a write arriving late would checkpoint against a
	// message of the session just closed
	ag.sessionID, ag.runner, ag.steering, ag.turnMsgID = "", nil, nil, ""
	ag.textBuf, ag.reasonBuf, ag.tools = "", "", nil
	ag.sessIn, ag.sessOut, ag.lastIn, ag.sessCost = 0, 0, 0, 0
	ag.cacheRead, ag.cacheBilled = 0, 0
	ag.compactFails, ag.compacted = 0, false
	ag.brain, ag.brainGlobs, ag.memAuto, ag.pin = "", nil, 0, promptPin{}
	ag.hookCtx = ""      // the new session captures its own
	ag.brainWrites = nil // the new session inherits no file this one wrote
	ag.dropPin()
	ag.todoInProgress, ag.todoSteps, ag.todoPending, ag.todoNag = "", 0, false, ""
	ag.goal, ag.goalPending, ag.goalRuns = nil, "", 0
	// the loop ran in the conversation leaving view: it stops without a word
	if ag.loopStop != nil {
		ag.loopStop()
	}
	ag.loop, ag.loopStop = nil, nil
	if open != nil {
		open()
	}
	ag.mu.Unlock()
	// only background reports wait at idle: the user hears of them, as of
	// one that arrives after the session moved on
	h.notifyReports(reports)
	h.refreshPending()
	if prev != "" {
		// the payload names the session that ended: the live one is the next
		h.consultHooks("session.close", map[string]any{"session_id": prev})
		h.EmitAsync("session.close", map[string]any{"id": prev})
		h.reflectIfDue(prev)
		if h.DB != nil {
			h.DB.Touch()
		}
	}
	return nil
}

// setTranscript replaces the chat (new, resume, rewind), which opens at its
// tail: a place scrolled to in the old one means nothing in the new.
func (h *Host) setTranscript(title string, tr []ChatMessage) {
	h.tMu.Lock()
	h.session.Title, h.session.Transcript, h.view = title, tr, nil
	h.trEpoch.Add(1)
	h.tMu.Unlock()
	h.scroll(ScrollBottom, "")
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
		if row.Role != string(fantasy.MessageRoleUser) || json.Unmarshal([]byte(row.PartsJSON), &m) != nil {
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
// are tool telemetry: a call's diff, run and summary, a step's thought time,
// a turn's tokens and duration.
func historyFrom(rows []*store.Message) ([]fantasy.Message, []ChatMessage) {
	var history []fantasy.Message
	var tr []ChatMessage
	toolRow := map[string]int{} // tool call id -> transcript index
	type telemetry struct {
		ID        string `json:"id"`
		Diff      string `json:"diff"`
		Run       string `json:"run"`
		Detail    string `json:"detail"`
		ThoughtMS int64  `json:"thought_ms"`
		TurnMS    int64  `json:"turn_ms"`
		In        int64  `json:"in"`
		Out       int64  `json:"out"`
		Ended     int64  `json:"ended"`
	}
	// A call's telemetry is stored as its tool ends, before the step's
	// messages, so a call takes the nearest one before it: a provider may
	// number ids per turn, and t1 then names many calls in one session.
	// NOTE: a cancelled step can leave telemetry without its call; a user
	// row clears what is pending, so only a call in that same turn reusing
	// the id could take it.
	pending := map[string]telemetry{}
	reasoning := -1 // the transcript index of the latest reasoning row
	reply := -1     // and of the latest reply, which a turn's stat belongs to
	for _, row := range rows {
		if row.Role == contextRole {
			continue
		}
		if row.Role == goalRole {
			var g goalRow
			if json.Unmarshal([]byte(row.PartsJSON), &g) == nil {
				if m, ok := goalNotice(g); ok {
					tr = append(tr, m)
				}
			}
			continue
		}
		var tc telemetry
		if row.Role == "tool" && json.Unmarshal([]byte(row.PartsJSON), &tc) == nil {
			if tc.ID != "" {
				pending[tc.ID] = tc
				continue
			}
			if tc.ThoughtMS > 0 && reasoning >= 0 {
				tr[reasoning].Stat.ThoughtMS += tc.ThoughtMS
				continue
			}
			// a turn's readout, kept whole: the message rows carry no usage
			if tc.Ended > 0 && reply >= 0 {
				tr[reply].Stat = &TurnStat{In: tc.In, Out: tc.Out, ElapsedMS: tc.TurnMS, Ended: tc.Ended}
				continue
			}
		}
		if row.Role == string(fantasy.MessageRoleUser) {
			clear(pending)
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
		replyText := func() {
			if s := strings.TrimSpace(text.String()); s != "" {
				tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Text: s, Stat: &TurnStat{Ended: row.CreatedAt / 1000}})
				reply = len(tr) - 1
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
					tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowReasoning, Text: p.Text, Stat: &TurnStat{}})
				}
			case fantasy.ToolCallPart:
				replyText()
				toolRow[p.ToolCallID] = len(tr)
				tc := pending[p.ToolCallID]
				delete(pending, p.ToolCallID)
				tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: p.ToolName, Text: toolSummary(p.ToolName, p.Input), Label: toolLabel(p.Input), Status: CallOK, Diff: tc.Diff, Run: tc.Run, Detail: tc.Detail})
			case fantasy.ToolResultPart:
				if i, ok := toolRow[p.ToolCallID]; ok && p.Output != nil {
					out := agent.ToolOutput(p.Output)
					tr[i].Output = rowOutput(tr[i].Tool, out)
					tr[i].Status = toolStatus(tr[i].Tool, p.Output, false)
				}
			}
		}
		s := text.String()
		if m.Role == fantasy.MessageRoleUser {
			if _, ok := summaryOf(m); ok {
				tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowCompact, Text: "Compacted earlier messages"})
				continue
			}
			var tail string
			s, tail, _ = strings.Cut(s, agent.AttachMarker) // show what was typed
			if s = strings.TrimSpace(s); s != "" {
				tr = append(tr, ChatMessage{Role: fantasy.MessageRoleUser, Text: s})
			}
			for _, n := range reportHeads(tail) {
				tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: n.Text, Status: n.Status})
			}
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Text: s})
			reply = len(tr) - 1
		}
	}
	return history, tr
}

const titleSystem = `Name the coding conversation below for a session list, from the user's first message alone. Do not answer or continue it. Reply with the title only: 3 to 6 words naming the task, file and identifier names kept exact, no quotes, no trailing period, in the language the user wrote in.`

// autoTitle names a new session from its first user message with a short
// model call in the background; any failure keeps the first-line title. No
// UI means no title: a zemi run exits before the call would answer.
func (h *Host) autoTitle(sid, user string) {
	if !h.Settings.Bool("session.auto_title") || h.hooks() == nil {
		return
	}
	if h.DB == nil {
		return
	}
	h.GoSafe("session title", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := h.generate(ctx, sid, titleSystem, "<message>\n"+truncRunes(user, 2000)+"\n</message>", nil, nil, "titles")
		if err != nil {
			h.Log.Debug("title generation failed", "origin", "session", "error", err)
			return
		}
		first, _, _ := strings.Cut(strings.TrimSpace(concatText(res.Response.Content)), "\n")
		title := sanitize.String(strings.Trim(strings.TrimSpace(first), `"'.# `))
		if title == "" {
			return
		}
		if err := h.DB.RenameSession(sid, title); err != nil {
			return
		}
		h.setLiveTitle(sid, title)
	})
}

// RenameSession names a session: the live one when id is "", else the stored
// row id. One path for /rename, the sessions picker's row action and
// zemi.session.rename. An empty title opens the one-line prompt the text of
// it is typed into.
func (h *Host) RenameSession(id, title string) error {
	if h.DB == nil {
		return errNoStore
	}
	if id = cmp.Or(id, h.AgentSessionID()); id == "" {
		return Usagef("no session to rename; send a message or /new first")
	}
	if strings.TrimSpace(title) == "" {
		ui, err := h.Screen()
		if err != nil {
			return err
		}
		sess, err := h.DB.GetSession(id)
		if err != nil {
			return h.noSession(id, err)
		}
		// the command the typed title runs: the live session's own /rename,
		// else the one the sessions picker's row action runs
		run := "sessions rename " + id + " "
		if id == h.AgentSessionID() {
			run = "rename "
		}
		ui.OpenOverlay(Overlay{
			ID: "rename", Title: "Rename session", Origin: lua.OriginBuiltin, Input: true,
			Default: clipRunes(sess.Title, 60), Run: run,
		})
		return nil
	}
	title = clipRunes(strings.TrimSpace(sanitize.String(title)), 60)
	if title == "" {
		return Usagef("usage: rename <title>")
	}
	if err := h.DB.RenameSession(id, title); err != nil {
		return h.noSession(id, err)
	}
	h.setLiveTitle(id, title)
	return nil
}

// setLiveTitle names the live session when id is it, and marks the sessions
// picker stale either way: the stored row is the caller's to write.
func (h *Host) setLiveTitle(id, title string) {
	h.recentStale.Store(true)
	if h.AgentSessionID() != id {
		return
	}
	h.tMu.Lock()
	h.session.Title = title
	h.tMu.Unlock()
	h.refresh()
}

// noSession is a failed session write, an unknown id named as such.
func (h *Host) noSession(id string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return Usagef("no session %q in this project", id)
	}
	return err
}

// renameCommands is /rename: a title given renames the live session at once,
// none opens the prompt to type one.
func (h *Host) renameCommands() []*Command {
	return []*Command{{
		Name: "rename", NArgs: NArgsOptional, Desc: "name this session", GoFn: func(h *Host, args string) error {
			return h.RenameSession("", strings.TrimSpace(args))
		},
	}}
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

func (h *Host) sessionNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("id", L.NewFunction(func(L *glua.LState) int {
		L.Push(glua.LString(h.AgentSessionID()))
		return 1
	}))
	tb.RawSetString("title", L.NewFunction(func(L *glua.LState) int {
		L.Push(glua.LString(h.Session().Title))
		return 1
	}))
	tb.RawSetString("rename", L.NewFunction(func(L *glua.LState) int {
		_ = h.RenameSession("", L.CheckString(1))
		return 0
	}))
	tb.RawSetString("messages", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		sid := h.AgentSessionID()
		if sid != "" && h.DB != nil {
			msgs, _ := h.DB.Messages(sid)
			for _, m := range msgs {
				row := L.NewTable()
				row.RawSetString("id", glua.LString(m.ID))
				row.RawSetString("role", glua.LString(m.Role))
				// the text parts joined: reading a reply must not mean
				// decoding the provider's wire shape by hand
				var msg fantasy.Message
				if json.Unmarshal([]byte(m.PartsJSON), &msg) == nil {
					row.RawSetString("text", glua.LString(concatText(msg.Content)))
				}
				row.RawSetString("json", glua.LString(m.PartsJSON))
				out.Append(row)
			}
		}
		L.Push(out)
		return 1
	}))
	h.sessionExtraFns(L, tb)
	return tb
}
