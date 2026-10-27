// session.go is what the main chat keeps per process: the composer history
// read from the log, the transcript window swap a rewind does, and the
// stored-row decoding a subagent run's transcript rebuilds from.

package hostapi

import (
	"cmp"
	"encoding/json/v2"
	"fmt"
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
	rows, err := h.DB.LogKindBack(kindUser, limit)
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
// rewind), which opens at its tail: a place scrolled to in the old one means
// nothing in the new.
func (h *Host) setTranscriptFromLog() {
	if h.DB == nil {
		return
	}
	rows, err := h.DB.LogTail(chatWindowRows)
	if err != nil {
		h.Log.Warn("chat window not rebuilt", "origin", "rewind", "error", err)
		return
	}
	h.SetTranscript(logRows(rows))
}

// SetTranscript replaces the chat, which opens at its tail: the swap a
// rewind's cut does to the window.
func (h *Host) SetTranscript(tr []ChatMessage) {
	h.tMu.Lock()
	h.session.Transcript, h.view = tr, nil
	h.trEpoch.Add(1)
	h.tMu.Unlock()
	// the loaded pages and the stamping cursor speak the cut log's shape, and
	// the window is stamped whole from the log
	h.msgMu.Lock()
	h.msgOld, h.msgFrom = nil, -1
	h.msgMu.Unlock()
	h.stampReset(tr)
	h.scroll(ScrollBottom, "")
	h.refresh()
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
		for w < len(tr) && !stampable(tr[w]) {
			w++ // a notice, a thought or an error row: the log row it follows pairs past it
		}
		if w >= len(tr) {
			break
		}
		m := tr[w]
		if (row.Kind == kindTool) != (m.Kind == RowTool) ||
			row.Kind != kindTool && (row.Kind == kindUser) != (m.Role == fantasy.MessageRoleUser) {
			break // the log is ahead of the flush that draws its row: wait for it
		}
		if row.Kind != kindTool && row.Text != m.Text {
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
// rewind's swap, the boot seed.
func (h *Host) stampReset(tr []ChatMessage) {
	h.tMu.Lock()
	h.stampRow = len(tr)
	h.stampFrom = h.stampedTail(tr)
	h.tMu.Unlock()
}

// stampable reports whether a transcript row comes from a log row the
// stamping can name: the user's words, a reply's stretch, a tool call.
// Notices, thoughts, errors and !commands have no log row of their own.
func stampable(m ChatMessage) bool {
	if m.Kind == RowTool {
		return true
	}
	return m.Kind == "" && m.Text != ""
}

// logRows converts log rows into the transcript rows the chat draws: a user
// or agent message its text, a tool call and its echo one row, a work report
// a notice naming the run. Notes draw nothing: zoom reaches them.
func logRows(rows []store.LogEntry) []ChatMessage {
	tr := make([]ChatMessage, 0, len(rows))
	for i, row := range rows {
		switch row.Kind {
		case kindUser:
			if s := strings.TrimSpace(row.Text); s != "" {
				tr = append(tr, ChatMessage{Role: fantasy.MessageRoleUser, Text: s, Seq: row.Seq, SentAt: stampMS(row.CreatedAt)})
			}
		case kindAgent:
			if s := strings.TrimSpace(row.Text); s != "" {
				tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Text: s, Seq: row.Seq})
			}
		case kindTool:
			name := rowTool(row)
			var out string
			var status CallStatus
			if j := i + 1; j < len(rows) && rows[j].Kind == kindEcho && rowTool(rows[j]) == name {
				out = shortOutput(rows[j].Text)
				status = CallOK
				var meta struct {
					Ok bool `json:"ok"`
				}
				if json.Unmarshal([]byte(rows[j].MetaJSON), &meta) == nil && !meta.Ok {
					status = CallErr
				}
			}
			tr = append(tr, ChatMessage{
				Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: tools.ToolName(name),
				Text: toolSummary(name, row.Text), Label: toolLabel(row.Text), Status: cmp.Or(status, CallOK),
				Output: out, Seq: row.Seq,
			})
		case kindWork:
			var meta struct {
				Run  string `json:"run"`
				Name string `json:"name"`
			}
			_ = json.Unmarshal([]byte(row.MetaJSON), &meta)
			tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: "[" + meta.Name + "] " + firstLine(row.Text), Run: meta.Run, Seq: row.Seq})
		case kindEcho, kindNote:
		}
	}
	return tr
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// stampMS is a log stamp as unix ms, 0 when it does not parse.
func stampMS(stamp string) int64 {
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return 0
	}
	return at.UnixMilli()
}

// agoStamp is how long ago a log stamp is, as the lists say it.
func agoStamp(stamp string) string {
	if ms := stampMS(stamp); ms > 0 {
		return ago(ms)
	}
	return ""
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
	tb.RawSetString("rewind", L.NewFunction(func(L *glua.LState) int {
		n, err := h.RewindTo(L.CheckInt64(1))
		if err != nil {
			return luaResult(L, err)
		}
		L.Push(glua.LNumber(n))
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
