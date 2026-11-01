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

// LastReply reads the turn's own words off the chat window's tail: the last
// plain reply above the user's prompt, "" when the turn said nothing.
func (h *Host) LastReply() string {
	h.tMu.RLock()
	defer h.tMu.RUnlock()
	for _, m := range slices.Backward(h.session.Transcript) {
		if m.Kind != RowMessage {
			continue
		}
		switch m.Role {
		case fantasy.MessageRoleUser:
			return ""
		case fantasy.MessageRoleAssistant:
			if s := strings.TrimSpace(m.Text); s != "" {
				return s
			}
		}
	}
	return ""
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
	h.pendRows = nil
	h.metaWatch = nil
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
// wrote pair with nothing here: they buffer in pendRows for SyncChat to
// splice in at their place, and the pairing cursor passes them like the
// kinds it pairs with nothing.
func (h *Host) stampLogSeqsLocked() {
	if h.logWipedLocked() {
		// the log under this turn was reset: reload now, so the turn's next
		// rows pair with the log it really writes. What belonged to the chat
		// drops off this goroutine: a flush calls here under ag.mu, and tMu
		// must never wait on it.
		if h.reloadChatLocked() {
			h.GoSafe("chat dropped by the writer", h.WriterResetEnded)
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
	for i := 0; i < len(rows); i++ {
		row := rows[i]
		if row.Origin != h.origin {
			// a row this window did not write: never this transcript's to stamp
			h.pendRows = append(h.pendRows, row)
			consumed++
			continue
		}
		if row.Kind != store.KindUser && row.Kind != store.KindAgent && row.Kind != store.KindTool {
			// a result, report, note, thought, error or shell row pairs with
			// nothing: the cursor passes it
			consumed++
			continue
		}
		for w < len(tr) && (!stampable(tr[w]) || tr[w].Seq != NoSeq) {
			w++ // a notice, a thought or an error row pairs past it; a spliced row is named already
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
			if strings.HasPrefix(m.Text, row.Text) {
				// the message split into sibling log rows (the 8k chat split):
				// this row stands for the first chunk and names its message;
				// the siblings ride it, by their split mark where one is
				// written and by their text inside the row's own otherwise
				for i+1 < len(rows) && rows[i+1].Kind == row.Kind &&
					(splitMarked(rows[i+1]) || strings.Contains(m.Text, rows[i+1].Text)) {
					i++
					consumed++
				}
				if m.Seq != row.Seq {
					stamps = append(stamps, stamp{at: w, seq: row.Seq})
				}
				w++
				consumed++
				continue
			}
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

// ChatWiped reports whether another window reset the shared log under this
// window while its turn runs: the row the transcript last stamped no longer
// is the row the store holds there. Read-only on purpose — the reload stays
// on the flush path, whose lock order it needs; a count cannot see a wipe
// whose regrown rows cover it, and a quiet hold has unstamped rows besides.
func (h *Host) ChatWiped() bool {
	if h.DB == nil {
		return false
	}
	h.tMu.RLock()
	defer h.tMu.RUnlock()
	return h.logWipedLocked()
}

// WriterResetEnded ends this window's work after the writing window reset the
// shared log: the turn dies with the chat it ran in, the view reloads.
func (h *Host) WriterResetEnded() {
	h.chatDropped()
	// the chat the turn worked in is gone: its work would write rows a
	// wiped log no longer holds, so the turn ends with it
	h.AgentCancel()
	// a shown ask belonged to the wiped chat: dismiss it, or it outlives
	// the turn and a later answer runs a tool into the dead chat
	if ui := h.hooks(); ui != nil {
		ui.DismissAsk()
	}
	h.refreshMessages()
	h.scroll(ScrollBottom, "")
	h.refresh()
	h.Notify(slog.LevelInfo, "Chat reset by the writable window", "this window reloaded from the shared log")
}

// SyncChat pulls what other windows appended to the shared log into this
// window's transcript, on the config watch's idle tick and on every stream
// frame the writing window pushes: two windows on one
// repository are the one chat it keeps, and the log's newest word is the
// truth. A live turn or run owns the tail, so the tick's own AgentActive
// guard covers it. A log that shrank under this window, or whose row this
// window stamped now reads differently, is another window's reset: the
// window reloads whole, the way its own reset left it.
func (h *Host) SyncChat() {
	if h.DB == nil || h.DB.Origin == "" {
		return
	}
	// a goal another window set that the kv no longer holds (met or stopped
	// where it was set, or replaced by a newer goal) leaves this window's copy
	// stale: drop it, with no judge run here
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
	// a goal another window set joins this window live, so the chain can move
	// here with the write: the goal runs wherever the lease holds
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
		h.WriterResetEnded()
		return
	}
	h.stampLogSeqsLocked()
	pend := h.pendRows
	h.pendRows = nil
	if len(pend) > 0 {
		// the pairing buffered copies, and a reply's readout lands in its
		// row's meta after that: re-read the span, then splice what the log
		// holds now
		if fresh, err := h.DB.LogFrom(pend[0].Seq, int(pend[len(pend)-1].Seq-pend[0].Seq)+1); err == nil {
			pend = pend[:0]
			for _, row := range fresh {
				if row.Origin != h.origin {
					pend = append(pend, row)
				}
			}
		}
		// Session() hands out snapshots of this array under a
		// never-written-in-place contract, and an insert lands mid-array
		// whenever a spliced row's seq sorts before the stamped tail: build
		// the spliced slice fresh, the way a whole replacement does
		tr := slices.Clone(h.session.Transcript)
		for _, m := range logRows(pend) {
			// splice at the row's place in the log, so the window draws the
			// order a restart reads: past the pairing's cursor the rows are
			// unclaimed and their seqs all sort above the buffered span, so
			// the scan stops at the first of them - stepping over drew the
			// burst after own rows it precedes
			at := len(tr)
			for i, o := range tr {
				if o.Seq != NoSeq && o.Seq > m.Seq || i >= h.stampRow && o.Seq == NoSeq && stampable(o) {
					at = i
					break
				}
			}
			tr = slices.Insert(tr, at, m)
			if at <= h.stampRow {
				h.stampRow++ // the pairing's row moved down with the insert
			}
			if m.Role == fantasy.MessageRoleAssistant && m.Kind == RowMessage && m.Stat == nil {
				// the readout lands on the reply row's meta only at its
				// turn's end, after the row itself spliced in
				h.metaWatch = append(h.metaWatch, m.Seq)
			}
		}
		h.session.Transcript = h.trimTranscript(tr)
		h.trEpoch.Add(1)
	}
	if len(h.metaWatch) > 0 {
		h.watchMetaLocked()
	}
	h.tMu.Unlock()
	h.agents.mu.Unlock()
	if len(pend) > 0 {
		// another window may have written the list: the snapshot reloads on
		// the next render, or this window's block freezes at its own last
		// write forever
		h.todosStale.Store(true)
		h.refresh()
		// a draft in the composer can be answered by what just arrived: say
		// it once per draft, so a chatty window cannot re-toast an unchanged one
		if draft := h.ComposerText(); strings.TrimSpace(draft) != "" && draft != h.draftToasted {
			h.composerMu.Lock()
			h.draftToasted = draft
			h.composerMu.Unlock()
			h.Notify(slog.LevelInfo, "The writable window sent a message", "your draft is kept")
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
// carrying its seq: the kind and text identity the pairing stamps by. A
// longer message logs as sibling chunks and the transcript row holds the
// whole, so the first chunk matches as its prefix - a reset's regrown row
// would have to open with the same 8000 characters to pass as the same.
func logRowMatches(row store.LogEntry, m ChatMessage) bool {
	text := strings.TrimSpace(row.Text)
	switch row.Kind {
	case store.KindUser:
		return m.Role == fantasy.MessageRoleUser && m.Kind == RowMessage &&
			(text == strings.TrimSpace(m.Text) || firstChunkOf(text, m.Text))
	case store.KindAgent:
		return m.Role == fantasy.MessageRoleAssistant && m.Kind == RowMessage &&
			(text == strings.TrimSpace(m.Text) || firstChunkOf(text, m.Text))
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
	case store.KindNote:
		// the chapter mark the transcript carries: first line, as it draws
		return m.Kind == RowNotice && firstLine(text) == strings.TrimSpace(m.Text)
	case store.KindNotice:
		// a status notice: first line, as it draws
		return m.Kind == RowNotice && firstLine(text) == strings.TrimSpace(m.Text)
	default: // an echo: no transcript row carries its seq
		return false
	}
}

// firstChunkOf reports whether text opens a message the transcript holds
// whole: the chat split's first sibling, shorter than the message it starts.
func firstChunkOf(text, whole string) bool {
	whole = strings.TrimSpace(whole)
	return text != whole && strings.HasPrefix(whole, text)
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
	h.pendRows = nil
	h.metaWatch = nil
	h.trEpoch.Add(1)
	return true
}

// stampable reports whether a transcript row comes from a log row the
// stamping can name: the user's words, a reply's stretch, a tool call.
// Notices, thoughts, errors and shell commands draw rows the stamping pairs past:
// the others read theirs back by seq in logRows, and a notice arrives
// stamped by its own append.
func stampable(m ChatMessage) bool {
	if m.Kind == RowTool {
		return true
	}
	return m.Kind == "" && m.Text != ""
}

// logRows converts log rows into the transcript rows the chat draws: a user
// or agent message its text, a tool call and its echo one row, a work report
// a notice naming the run, a thought, an error or a shell command its own row.
// Notes draw nothing: zoom reaches them.
func logRows(rows []store.LogEntry) []ChatMessage {
	return logRowsFor(rows, false)
}

// logRowsFor builds the transcript rows a log range draws. browse adds the
// echoes nothing consumed — an attachment's content, a shell command's full
// output — as dim rows: the transcript folds them into the row they belong
// to, the browser is where the log's whole truth lives.
func logRowsFor(rows []store.LogEntry, browse bool) []ChatMessage {
	// the echo each tool row answers: by the call id both metas carry, else
	// the oldest unconsumed echo of the same name, the order parallel calls
	// of one tool answer in; a repeated id answers in log order, so the
	// queues hold every echo and a call takes the oldest past itself
	echo := make(map[string][]int, len(rows))
	fifo := make(map[string][]int, len(rows))
	for i, row := range rows {
		if row.Kind != store.KindEcho {
			continue
		}
		var meta struct {
			Tool string `json:"tool"`
			ID   string `json:"id"`
		}
		_ = json.Unmarshal([]byte(row.MetaJSON), &meta)
		if meta.ID != "" {
			echo[meta.ID] = append(echo[meta.ID], i)
		} else {
			fifo[meta.Tool] = append(fifo[meta.Tool], i)
		}
	}
	// nextEcho pops the oldest echo below the call: rows above it belong to
	// calls the window shed
	next := func(q []int, i int) ([]int, int) {
		for len(q) > 0 && q[0] <= i {
			q = q[1:]
		}
		if len(q) == 0 {
			return nil, -1
		}
		return q[1:], q[0]
	}
	tr := make([]ChatMessage, 0, len(rows))
	for i := 0; i < len(rows); i++ {
		row := rows[i]
		if splitMarked(row) {
			// the chat logged a longer message as siblings: the transcript
			// holds one row for it, so the rebuild folds them back together
			var b strings.Builder
			b.WriteString(row.Text)
			for i+1 < len(rows) && splitMarked(rows[i+1]) && rows[i+1].Kind == row.Kind {
				i++
				b.WriteString(rows[i].Text)
			}
			row.Text = b.String()
			// the readout settles on the group's last sibling: the folded
			// row carries it, not the bare meta its first chunk holds
			row.MetaJSON = rows[i].MetaJSON
		}
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
			var call struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal([]byte(row.MetaJSON), &call)
			j := -1
			if call.ID != "" {
				echo[call.ID], j = next(echo[call.ID], i)
			}
			if j < 0 {
				fifo[name], j = next(fifo[name], i)
			}
			var out string
			var status CallStatus
			var diff, run, job string
			if j > i {
				e := rows[j]
				var em echoRow
				_ = json.Unmarshal([]byte(e.MetaJSON), &em)
				out = rowOutput(name, e.Text)
				status = cmp.Or(CallStatus(em.Status), statusOf(em.OK))
				diff, run, job = em.Diff, em.Run, em.Job
			} else if j2 := i + 1; j2 < len(rows) && rows[j2].Kind == store.KindUser {
				// a turn killed between the call and its echo (a window
				// quitting mid-turn): the call never answered
				status = CallErr
			}
			tr = append(tr, ChatMessage{
				Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: tools.ToolName(name),
				Text: toolSummary(name, row.Text), Label: toolLabel(row.Text), Status: cmp.Or(status, CallOK),
				Diff: diff, Run: run, Job: job, Output: out, Seq: row.Seq,
			})
		case store.KindWork:
			var meta struct {
				Run    string `json:"run"`
				Name   string `json:"name"`
				Agent  string `json:"agent,omitempty"`
				Status string `json:"status"`
			}
			_ = json.Unmarshal([]byte(row.MetaJSON), &meta)
			// the head is in the logged text, where work wrote it: the row
			// draws its first line, the same line the live window drew
			tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: firstLine(row.Text), Status: workStatus(meta.Status), Run: meta.Run, Tool: tools.ToolName(cmp.Or(meta.Agent, meta.Name)), Seq: row.Seq})
		case store.KindEcho:
		case store.KindNote:
			// a chapter mark the model reads, so the chat shows where; the
			// wipe's own system note stays off the transcript
			var meta struct {
				System bool `json:"system"`
			}
			_ = json.Unmarshal([]byte(row.MetaJSON), &meta)
			if meta.System {
				continue
			}
			tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: messageHead(row.Text), Seq: row.Seq})
		case store.KindNotice:
			var meta struct {
				Detail string `json:"detail"`
			}
			_ = json.Unmarshal([]byte(row.MetaJSON), &meta)
			tr = append(tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: messageHead(row.Text), Detail: meta.Detail, Seq: row.Seq})
		}
	}
	if browse {
		var left []int
		for _, q := range echo {
			left = append(left, q...)
		}
		for _, q := range fifo {
			left = append(left, q...)
		}
		slices.Sort(left)
		if len(left) > 0 {
			notice := func(i int) ChatMessage {
				return ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: messageHead(rows[i].Text), Detail: rows[i].Text, Tool: tools.ToolName(rowTool(rows[i])), Seq: rows[i].Seq}
			}
			merged := make([]ChatMessage, 0, len(tr)+len(left))
			li := 0
			for _, m := range tr {
				for li < len(left) && rows[left[li]].Seq < m.Seq {
					merged = append(merged, notice(left[li]))
					li++
				}
				merged = append(merged, m)
			}
			for ; li < len(left); li++ {
				merged = append(merged, notice(left[li]))
			}
			tr = merged
		}
	}
	return tr
}

func firstLine(s string) string {
	// a logged block opens with the newline its writer joined it with: the
	// first line is the first non-empty one, or every notice on it draws blank
	line, _, _ := strings.Cut(strings.TrimLeft(s, "\n"), "\n")
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
	if m == (replyStat{}) {
		return nil // the split mark rides a split reply's meta alone, before its readout lands
	}
	return &TurnStat{Model: m.Model, ElapsedMS: m.TurnMS, Out: m.Out, ThoughtMS: m.ThoughtMS, Ended: m.Ended}
}

// watchMetaLocked carries the readout a late settle wrote onto the log onto
// the reply rows a splice already drew: a reply's row lands when its text
// does, its readout only at its turn's end, so a window that spliced the row
// in between would draw it without its telemetry forever. tMu held.
func (h *Host) watchMetaLocked() {
	// a split reply's readout settles on its last sibling, past the seq its
	// row names: the read reaches the log's end or it stops short of it
	count, err := h.DB.LogCount()
	if err != nil || count < h.metaWatch[0] {
		return
	}
	fresh, err := h.DB.LogFrom(h.metaWatch[0], int(count-h.metaWatch[0])+1)
	if err != nil {
		return
	}
	read := make(map[int64]*TurnStat, len(fresh))
	start := int64(-1)
	for _, row := range fresh {
		stat := replyMeta(row.MetaJSON)
		if stat != nil {
			read[row.Seq] = stat
		}
		if row.Kind != store.KindAgent || !splitMarked(row) {
			start = -1
			continue
		}
		if start < 0 {
			start = row.Seq
		}
		// the transcript's row names the group's first chunk, the readout
		// settles on its last: let the watched row find it
		if stat != nil {
			read[start] = stat
		}
	}
	if len(read) > 0 {
		tr := h.session.Transcript
		clone := false
		for i := range tr {
			if stat := read[tr[i].Seq]; stat != nil && tr[i].Stat == nil {
				if !clone { // readers share the array: replace it, never write in place
					tr = slices.Clone(tr)
					clone = true
				}
				tr[i].Stat = stat
			}
		}
		if clone {
			h.session.Transcript = tr
			h.trEpoch.Add(1)
		}
		h.metaWatch = slices.DeleteFunc(h.metaWatch, func(seq int64) bool { return read[seq] != nil })
	}
	// NOTE: a readout that never lands (its writer died before the settle)
	// stays watched; the cap bounds that to the last 64 replies
	if len(h.metaWatch) > 64 {
		h.metaWatch = h.metaWatch[len(h.metaWatch)-64:]
	}
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
			// show what was typed; the model's markup drew its work row where
			// it was delivered, which the log's own rows rebuild
			if shown, _, _ := userRowsFor(text.String()); shown != "" {
				tr = append(tr, ChatMessage{Role: fantasy.MessageRoleUser, Text: shown, ID: row.ID, SentAt: row.CreatedAt})
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
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
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
