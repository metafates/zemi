// session.go is what the main chat keeps per process: the composer history
// read from the log, the transcript window swap a reset does, and the
// stored-row decoding a subagent run's transcript rebuilds from.

package hostapi

import (
	"cmp"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// errTurnRunning refuses what would cut the conversation mid-turn.
var errTurnRunning = Usagef("a turn is running; esc twice interrupts it")

// InputHistory returns the last limit messages the user typed in this
// project, oldest first, for the composer history: the log's user rows.
func (h *Host) InputHistory(limit int) []string {
	if h.DB == nil || limit <= 0 {
		return nil
	}
	rows, err := h.DB.LogKindBack(store.KindUser, limit)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		if s := strings.TrimSpace(row.Text); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// setTranscriptFromLog replaces the chat window with the log's tail (a
// reset), which opens at its tail: a place scrolled to in the old one means
// nothing in the new.
func (h *Host) setTranscriptFromLog() {
	if h.DB == nil {
		return
	}
	rows, err := h.DB.LogTail(chatWindowRows)
	if err != nil {
		h.Log.Warn("chat window not rebuilt", "origin", "reset", "error", err)
		return
	}
	h.SetTranscript(logRows(rows))
}

// SetTranscript replaces the chat, which opens at its tail: the swap a
// reset's wipe does to the window.
func (h *Host) SetTranscript(tr []ChatMessage) {
	h.tMu.Lock()
	h.session.Transcript, h.view = tr, nil
	h.trEpoch.Add(1)
	h.foreignPend = nil
	h.tMu.Unlock()
	// the loaded pages and the stamping cursor speak the cut log's shape, and
	// the window is stamped whole from the log
	h.msgMu.Lock()
	h.msgOld, h.msgFrom = nil, -1
	h.msgMu.Unlock()
	h.stampReset(tr)
	h.scroll(ScrollBottom, "")
	h.refresh()
	h.refreshMessages()
}

// stampedTail is the seq the transcript's stamping resumes from: the last
// logged row's, so the log rows after it are the unclaimed ones.
func (h *Host) stampedTail(tr []ChatMessage) int64 {
	for _, m := range slices.Backward(tr) {
		if m.Seq != NoSeq {
			return m.Seq + 1
		}
	}
	return 0
}

// stampLogSeqs fills the transcript rows' Seq from the log: the log's rows
// pair with the window's stampable ones in order, by kind - the log receives
// the same stretches in the same order, sometimes ahead of the flush that
// draws them, so an unpaired head waits for its row. stampRow is the window
// row the pairing sits at (stampCursor moves it when rows shift); rows before
// it are claimed, whatever seq they carry, the log's own 0 included.
func (h *Host) stampLogSeqs() {
	if h.DB == nil {
		return
	}
	h.tMu.Lock()
	defer h.tMu.Unlock()
	h.stampLogSeqsLocked()
}

// stampLogSeqsLocked is stampLogSeqs with tMu held. Rows another window
// wrote pair with nothing here: they buffer in foreignPend for syncChat to
// splice in at their place, and the pairing cursor passes them like the
// kinds it pairs with nothing.
func (h *Host) stampLogSeqsLocked() {
	if h.logWipedLocked() {
		// the log under this turn was reset: reload now, so the turn's next
		// rows pair with the log it really writes. What belonged to the chat
		// drops off this goroutine: a flush calls here under ag.mu, and tMu
		// must never wait on it.
		if h.reloadChatLocked() {
			h.GoSafe("chat dropped by another window", func() {
				h.chatDropped()
				h.refreshMessages()
				h.scroll(ScrollBottom, "")
				h.refresh()
				h.Notify(slog.LevelInfo, "Chat reset by another window", "this window reloaded from the shared log")
			})
		}
		return
	}
	tr := h.session.Transcript
	rows, err := h.DB.LogFrom(h.stampFrom, stampBatch)
	if err != nil || len(rows) == 0 {
		return
	}
	type stamp struct {
		at   int
		seq  int64
		gone bool // the row's log write failed: it draws, but names no message
	}
	var stamps []stamp
	w := min(h.stampRow, len(tr))
	consumed := 0
	for _, row := range rows {
		if row.Origin != h.origin {
			// another window's row: never this transcript's to stamp
			h.foreignPend = append(h.foreignPend, row)
			consumed++
			continue
		}
		if row.Kind != store.KindUser && row.Kind != store.KindAgent && row.Kind != store.KindTool {
			// a result, report, note, thought, error or shell row pairs with
			// nothing: the cursor passes it
			consumed++
			continue
		}
		for w < len(tr) && (!stampable(tr[w]) || tr[w].Foreign && tr[w].Seq != NoSeq) {
			w++ // a notice, a thought or an error row pairs past it; a spliced foreign row is named already
		}
		if w >= len(tr) {
			break
		}
		m := tr[w]
		if (row.Kind == store.KindTool) != (m.Kind == RowTool) ||
			row.Kind != store.KindTool && (row.Kind == store.KindUser) != (m.Role == fantasy.MessageRoleUser) {
			break // the log is ahead of the flush that draws its row: wait for it
		}
		if row.Kind != store.KindTool && row.Text != m.Text {
			// the log write for this row failed: the row draws unstamped and
			// the log row pairs with the next one
			stamps = append(stamps, stamp{at: w, gone: true})
			w++
			continue
		}
		if m.Seq != row.Seq {
			stamps = append(stamps, stamp{at: w, seq: row.Seq})
		}
		w++
		consumed++
	}
	if len(stamps) > 0 {
		tr = slices.Clone(tr)
		for _, s := range stamps {
			if s.gone {
				tr[s.at].Seq = NoSeq // names no message, now or ever
			} else {
				tr[s.at].Seq = s.seq
			}
		}
		h.session.Transcript = tr
	}
	h.stampRow, h.stampFrom = w, h.stampFrom+int64(consumed)
}

// stampCursor moves the pairing's window row when the transcript shifts
// under it: rows cut from the head or spliced out before it.
func (h *Host) stampCursor(cut int) {
	h.stampRow = max(h.stampRow-cut, 0)
}

// stampReset points the pairing past a window that is stamped whole: a
// reset's swap, the boot seed.
func (h *Host) stampReset(tr []ChatMessage) {
	h.tMu.Lock()
	h.stampRow = len(tr)
	h.stampFrom = h.stampedTail(tr)
	h.tMu.Unlock()
}

// SyncChat pulls what other windows appended to the shared log into this
// window's transcript, on the config watch's idle tick: two windows on one
// repository are the one chat it keeps, and the log's newest word is the
// truth. A live turn or run owns the tail, so the tick's own AgentActive
// guard covers it. A log that shrank under this window, or whose row this
// window stamped now reads differently, is another window's reset: the
// window reloads whole, the way its own reset left it.
func (h *Host) SyncChat() {
	if h.DB == nil || h.DB.Origin == "" {
		return
	}
	// a foreign goal the kv row no longer holds (met or stopped where it was
	// set, or replaced by a newer goal) leaves this window's copy stale: drop
	// it, with no judge run here
	if g, ok := h.Goal(); ok && g.Writer != "" && g.Writer != h.origin {
		stale := false
		if raw, err := h.DB.GetKV(goalKV); errors.Is(err, store.ErrNotFound) {
			stale = true
		} else if err == nil {
			var row Goal
			if json.Unmarshal([]byte(raw), &row) == nil && (row.SetAt != g.SetAt || row.Condition != g.Condition) {
				stale = true
			}
		}
		if stale {
			h.agents.mu.Lock()
			h.agents.goal = nil
			h.agents.mu.Unlock()
			h.refresh()
		}
	}
	// a goal another window set joins this window live, so the chain can
	// move here once the writer's heartbeat is gone: goalMine decides at the
	// check whether this window runs it
	if _, ok := h.Goal(); !ok {
		h.loadGoal()
		if _, ok := h.Goal(); ok {
			h.refresh()
		}
	}
	// the agents lock leads the transcript lock everywhere (the turn path
	// holds ag.mu across its appends), so the tick's splice never takes
	// ag.mu inside tMu: that order made a turn starting inside this section
	// freeze both sides of the pair
	h.agents.mu.Lock()
	h.tMu.Lock()
	count, err := h.DB.LogCount()
	if err != nil {
		h.tMu.Unlock()
		h.agents.mu.Unlock()
		return
	}
	if (count < h.stampFrom || h.logWipedLocked()) && h.reloadChatLocked() {
		// the log shrank under this window's cursor, or the row it stamped
		// reads differently: another window reset, its regrown rows included
		h.tMu.Unlock()
		h.agents.mu.Unlock()
		h.chatDropped()
		h.refreshMessages()
		h.scroll(ScrollBottom, "")
		h.refresh()
		h.Notify(slog.LevelInfo, "Chat reset by another window", "this window reloaded from the shared log")
		return
	}
	h.stampLogSeqsLocked()
	foreign := h.foreignPend
	h.foreignPend = nil
	active := h.agents.active
	if len(foreign) > 0 && active {
		// a turn owns the tail: its unstamped rows are the pairing's to
		// claim, not a splice's to step over - the next tick splices them
		h.foreignPend = foreign
		foreign = nil
	}
	if len(foreign) > 0 {
		// the pairing buffered copies, and a reply's readout lands in its
		// row's meta after that: re-read the span, then splice what the log
		// holds now
		if fresh, err := h.DB.LogFrom(foreign[0].Seq, int(foreign[len(foreign)-1].Seq-foreign[0].Seq)+1); err == nil {
			foreign = foreign[:0]
			for _, row := range fresh {
				if row.Origin != h.origin {
					foreign = append(foreign, row)
				}
			}
		}
		// Session() hands out snapshots of this array under a
		// never-written-in-place contract, and an insert lands mid-array
		// whenever a foreign row's seq sorts before the stamped tail: build
		// the spliced slice fresh, the way a whole replacement does
		tr := slices.Clone(h.session.Transcript)
		for _, row := range foreign {
			// splice at the row's place in the log, so the window draws the
			// order a restart reads
			at := len(tr)
			for i, m := range tr {
				if m.Seq != NoSeq && m.Seq > row.Seq {
					at = i
					break
				}
			}
			rows := logRows([]store.LogEntry{row})
			for i := range rows {
				rows[i].Foreign = true
			}
			tr = slices.Insert(tr, at, rows...)
			if at <= h.stampRow {
				h.stampRow++ // the pairing's row moved down with the insert
			}
		}
		h.session.Transcript = tr
		h.trEpoch.Add(1)
	}
	h.tMu.Unlock()
	h.agents.mu.Unlock()
	if len(foreign) > 0 {
		h.refresh()
		// a draft in the composer can be answered by what just arrived: say
		// it once per draft, so a chatty window cannot re-toast an unchanged one
		if draft := h.ComposerText(); strings.TrimSpace(draft) != "" && draft != h.draftToasted {
			h.composerMu.Lock()
			h.draftToasted = draft
			h.composerMu.Unlock()
			h.Notify(slog.LevelInfo, "Another window sent a message", "your draft is kept")
		}
	}
}

// logWipedLocked reports whether another window reset the shared log under
// this window: the row its transcript last stamped no longer is the row the
// store holds at that seq. A count cannot see a wipe that regrown rows
// already cover; the row's own words are the evidence.
func (h *Host) logWipedLocked() bool {
	var m ChatMessage
	found := false
	for _, c := range slices.Backward(h.session.Transcript) {
		if c.Seq != NoSeq {
			m, found = c, true
			break
		}
	}
	if !found {
		return false // nothing stamped: no identity to check
	}
	row, err := h.DB.LogAt(m.Seq)
	if err != nil {
		return true // the cursor names a seq the log no longer holds
	}
	return !logRowMatches(row, m)
}

// logRowMatches reports whether the store row still is the transcript row
// carrying its seq: the kind and text identity the pairing stamps by.
func logRowMatches(row store.LogEntry, m ChatMessage) bool {
	text := strings.TrimSpace(row.Text)
	switch row.Kind {
	case store.KindUser:
		return m.Role == fantasy.MessageRoleUser && m.Kind == RowMessage && text == strings.TrimSpace(m.Text)
	case store.KindAgent:
		return m.Role == fantasy.MessageRoleAssistant && m.Kind == RowMessage && text == strings.TrimSpace(m.Text)
	case store.KindTool:
		return m.Kind == RowTool && toolSummary(rowTool(row), row.Text) == m.Text
	case store.KindThought:
		return m.Kind == RowReasoning && row.Text == m.Text
	case store.KindError:
		return m.Kind == RowError && row.Text == m.Text
	case store.KindShell:
		return m.Kind == RowShell && row.Text == m.Text
	case store.KindWork:
		return m.Kind == RowNotice // its text needs the row's meta; the kind is identity enough
	default: // an echo or note: no transcript row carries their seq
		return false
	}
}

// reloadChatLocked swaps the chat window for the log's tail: what a reset
// leaves, this window's own or another window's. tMu held. False when the
// read failed, leaving the window as it was.
func (h *Host) reloadChatLocked() bool {
	rows, err := h.DB.LogTail(chatWindowRows)
	if err != nil {
		return false
	}
	h.session.Transcript, h.view = logRows(rows), nil
	h.msgMu.Lock()
	h.msgOld, h.msgFrom = nil, -1
	h.msgMu.Unlock()
	h.stampRow, h.stampFrom = len(h.session.Transcript), h.stampedTail(h.session.Transcript)
	h.foreignPend = nil
	h.trEpoch.Add(1)
	return true
}

// stampable reports whether a transcript row comes from a log row the
// stamping can name: the user's words, a reply's stretch, a tool call.
// Notices, thoughts, errors and !commands draw rows the stamping pairs past:
// a notice has no log row, the others read theirs back by seq in logRows.
func stampable(m ChatMessage) bool {
	if m.Kind == RowTool {
		return true
	}
	return m.Kind == "" && m.Text != ""
}

// logRows converts log rows into the transcript rows the chat draws: a user
// or agent message its text, a tool call and its echo one row, a work report
// a notice naming the run, a thought, an error or a !command its own row.
// Notes draw nothing: zoom reaches them.
func logRows(rows []store.LogEntry) []ChatMessage {
	tr := make([]ChatMessage, 0, len(rows))
	for i, row := range rows {
		switch row.Kind {
		case store.KindUser:
			if s := strings.TrimSpace(row.Text); s != "" {
				tr = append(tr, ChatMessage{Role: fantasy.MessageRoleUser, Text: s, Seq: row.Seq, SentAt: stampMS(row.CreatedAt)})
			}
		case store.KindAgent:
			if s := strings.TrimSpace(row.Text); s != "" {
				tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Text: s, Seq: row.Seq, Stat: replyMeta(row.MetaJSON)})
			}
		case store.KindThought:
			var meta struct {
				MS int64 `json:"ms"`
			}
			_ = json.Unmarshal([]byte(row.MetaJSON), &meta)
			tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowReasoning, Text: row.Text, Stat: &TurnStat{ThoughtMS: meta.MS}, Seq: row.Seq})
		case store.KindError:
			var meta struct {
				Cancelled bool `json:"cancelled"`
			}
			_ = json.Unmarshal([]byte(row.MetaJSON), &meta)
			m := ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowError, Text: row.Text, Seq: row.Seq}
			if meta.Cancelled {
				m.Status = CallCancelled
			}
			tr = append(tr, m)
		case store.KindShell:
			var meta struct {
				OK  bool   `json:"ok"`
				Out string `json:"out"`
			}
			_ = json.Unmarshal([]byte(row.MetaJSON), &meta)
			status := CallOK
			if !meta.OK {
				status = CallErr
			}
			tr = append(tr, ChatMessage{Role: fantasy.MessageRoleUser, Kind: RowShell, Text: row.Text, Detail: meta.Out, Status: status, Seq: row.Seq})
		case store.KindTool:
			name := rowTool(row)
			var out string
			var status CallStatus
			if j := i + 1; j < len(rows) && rows[j].Kind == store.KindEcho && rowTool(rows[j]) == name {
				out = shortOutput(rows[j].Text)
				status = CallOK
				var meta struct {
					Ok bool `json:"ok"`
				}
				if json.Unmarshal([]byte(rows[j].MetaJSON), &meta) == nil && !meta.Ok {
					status = CallErr
				}
			} else if j < len(rows) && rows[j].Kind == store.KindUser {
				// a turn killed between the call and its echo (a window
				// quitting mid-turn): the call never answered
				status = CallErr
			}
			tr = append(tr, ChatMessage{
				Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: tools.ToolName(name),
				Text: toolSummary(name, row.Text), Label: toolLabel(row.Text), Status: cmp.Or(status, CallOK),
				Output: out, Seq: row.Seq,
			})
		case store.KindWork:
			var meta struct {
				Run  string `json:"run"`
				Name string `json:"name"`
			}
			_ = json.Unmarshal([]byte(row.MetaJSON), &meta)
			tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: "[" + meta.Name + "] " + firstLine(row.Text), Run: meta.Run, Seq: row.Seq})
		case store.KindEcho, store.KindNote:
		}
	}
	return tr
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// replyMeta is the turn stat the reply row's meta carries, nil for a row
// that predates it or wrote none: the readout and the digest's thought draw
// from it.
func replyMeta(meta string) *TurnStat {
	if meta == "" {
		return nil
	}
	var m replyStat
	if json.Unmarshal([]byte(meta), &m) != nil {
		return nil
	}
	return &TurnStat{Model: m.Model, ElapsedMS: m.TurnMS, Out: m.Out, ThoughtMS: m.ThoughtMS, Ended: m.Ended}
}

// stampMS is a log stamp as unix ms, 0 when it does not parse.
func stampMS(stamp string) int64 {
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return 0
	}
	return at.UnixMilli()
}

// historyFrom decodes a run's stored messages into model history and
// transcript rows, the rows the live run drew. Rows that are not fantasy
// messages are tool telemetry: a call's diff, run and summary, a step's
// thought time, a turn's tokens and duration.
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
		Model     string `json:"model"`
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
				tr[reply].Stat = &TurnStat{In: tc.In, Out: tc.Out, ElapsedMS: tc.TurnMS, Ended: tc.Ended, Model: tc.Model}
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
		// what the calls of this message did beyond their text, as they were
		// stored; a row older than that column reads as its result says
		var meta map[string]rowMeta
		if row.RowMeta != "" {
			_ = json.Unmarshal([]byte(row.RowMeta), &meta)
		}
		var text strings.Builder
		// text streamed before a call is a row of its own above it, as the
		// live turn flushed it; reasoning never cuts a reply (ReasoningDelta)
		replyText := func() {
			if s := strings.TrimSpace(text.String()); s != "" {
				tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Text: s})
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
				if p.ToolName == string(tools.ToolPlan) {
					// the plan shows as a message above its row, as the live turn drew it
					if plan := strings.TrimSpace(planArg(p.Input)); plan != "" {
						tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Text: sanitize.String(plan)})
					}
				}
				toolRow[p.ToolCallID] = len(tr)
				tc := pending[p.ToolCallID]
				delete(pending, p.ToolCallID)
				rm := meta[p.ToolCallID]
				tr = append(tr, ChatMessage{
					Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: tools.ToolName(p.ToolName),
					Text: toolSummary(p.ToolName, p.Input), Label: toolLabel(p.Input), Status: cmp.Or(rm.Status, CallOK),
					Diff: tc.Diff, Run: cmp.Or(tc.Run, rm.Run), Job: rm.Job, Detail: tc.Detail,
				})
			case fantasy.ToolResultPart:
				if i, ok := toolRow[p.ToolCallID]; ok && p.Output != nil {
					out := agent.ToolOutput(p.Output)
					tr[i].Output = rowOutput(string(tr[i].Tool), out)
					if rm := meta[p.ToolCallID]; rm.Status != "" {
						tr[i].Status = rm.Status
					} else if agent.ErrResult(p.Output) {
						tr[i].Status = CallErr
					}
				}
			}
		}
		if m.Role == fantasy.MessageRoleUser {
			shown, heads, _ := userRowsFor(text.String()) // show what was typed
			if shown != "" {
				tr = append(tr, ChatMessage{Role: fantasy.MessageRoleUser, Text: shown, ID: row.ID, SentAt: row.CreatedAt})
			}
			for _, n := range heads {
				tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: n.Text, Status: n.Status})
			}
			continue
		}
		if s := strings.TrimSpace(text.String()); s != "" {
			tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Text: s})
			reply = len(tr) - 1
		}
	}
	return history, tr
}

// planArg is the plan markdown in a plan call's input, "" when absent.
func planArg(input string) string {
	var in struct {
		Plan string `json:"plan"`
	}
	if json.Unmarshal([]byte(input), &in) != nil {
		return ""
	}
	return in.Plan
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
		L.Push(glua.LString(store.MainSessionID))
		return 1
	}))
	tb.RawSetString("todos", L.NewFunction(func(L *glua.LState) int {
		var rows []any
		if h.DB != nil {
			todos, _ := h.DB.Todos(store.MainSessionID)
			for _, td := range todos {
				rows = append(rows, map[string]any{"content": td.Content, "status": td.Status, "active_form": td.ActiveForm})
			}
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))
	return tb
}
