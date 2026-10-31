// messages.go is the messages picker: the conversation as one list, a row
// per message, with the chat scrolling to whatever the selection names. It
// is how an earlier prompt or reply is found and copied.

package hostapi

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// messagesCommands is /messages: the conversation as one list, and the
// levels under it a list row opens.
func (h *Host) messagesCommands() []*Command {
	return []*Command{{
		Name: "messages", NArgs: NArgsAny, Desc: "read or copy a message",
		GoFn:       func(h *Host, args string, confirm bool) error { return h.MessagesCommand(args) },
		GoComplete: SubcommandCompleter([]string{"rail", "view"}, nil),
	}}
}

// MessagesCommand is /messages and the levels under it: no argument lists
// the conversation, "rail N" opens one run's rail, "view N" reads one row
// whole (a "#seq" names a loaded page's log row).
func (h *Host) MessagesCommand(args string) error {
	verb, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	switch verb {
	case "":
		return h.OpenMessages()
	case "rail":
		rest = strings.TrimSpace(rest)
		if n, err := strconv.Atoi(rest); err == nil {
			return h.openMessagesRail(n)
		}
		if seq, err := strconv.ParseInt(strings.TrimPrefix(rest, "#"), 10, 64); err == nil {
			return h.openMessagesRailSeq(seq)
		}
		return Usagef("/messages rail <row> (a row the list shows; opens the run behind it)")
	case "view":
		rest = strings.TrimSpace(rest)
		if n, err := strconv.Atoi(rest); err == nil {
			return h.openMessageView(n)
		}
		seq, err := strconv.ParseInt(strings.TrimPrefix(rest, "#"), 10, 64)
		if err != nil {
			return Usagef("/messages view <row> (a row the list shows; opens the row whole)")
		}
		return h.openLogView(seq)
	}
	return Usagef("usage: /messages [rail <row> | view <row>]")
}

// pickTr is the conversation the picker reads: the session's transcript plus,
// while a turn runs, the calls it has not settled yet, and the loaded pages
// of log rows the chat window shed. The chat draws those on its live rail
// and marks them call = -i for the i-th of them, so a live call's anchor is
// negative and bands its own live rows. They extend the run the
// transcript's activity tail is in; a tail that is a message (a flushed
// text row) leaves them a run of their own, which the chat anchors by the
// first call's -1. The loaded pages follow the live calls in the negative
// rows: the chat holds none of them, so their items open views by log seq.
type pickTr struct {
	tr   []ChatMessage
	live []ChatMessage
	old  []ChatMessage
}

// at is the row idx names: 1.. for transcript rows, -1.. for the live calls
// and then the loaded pages.
func (p pickTr) at(idx int) ChatMessage {
	if idx == 0 {
		return ChatMessage{}
	}
	if idx > 0 {
		if idx <= len(p.tr) {
			return p.tr[idx-1]
		}
		return ChatMessage{}
	}
	if n := -idx; n <= len(p.live) {
		return p.live[n-1]
	}
	if i := -idx - len(p.live) - 1; i < len(p.old) {
		return p.old[i]
	}
	return ChatMessage{}
}

// pickerTranscript is the conversation as the chat draws it, read under one
// lock: the session's rows, or a shown run's, plus the live turn's calls.
func (h *Host) pickerTranscript() pickTr {
	h.agents.mu.Lock()
	defer h.agents.mu.Unlock()
	return h.pickerTranscriptLocked()
}

// withBrowseEchoes merges the log's never-consumed echoes inside the
// transcript's own span into the picker's rows: the transcript folds an
// attachment's content or a !command's output into the row it belongs to,
// and the browser is where the log's whole truth lives. The paged-back
// pages get theirs from logRowsFor; these would wait for a thousand-row cut.
func (h *Host) withBrowseEchoes(p pickTr) pickTr {
	if h.DB == nil || len(p.tr) == 0 {
		return p
	}
	first, last := int64(-1), int64(-1)
	for _, m := range p.tr {
		if m.Seq == NoSeq {
			continue
		}
		if first < 0 {
			first = m.Seq
		}
		last = m.Seq
	}
	if first < 0 || last < first {
		return p
	}
	rows, err := h.DB.LogFrom(first, int(last-first+1))
	if err != nil || len(rows) == 0 {
		return p
	}
	full := logRowsFor(rows, true)
	plain := logRows(rows)
	if len(full) == len(plain) {
		return p // nothing unpaired inside the span
	}
	seen := make(map[int64]bool, len(plain))
	for _, m := range plain {
		seen[m.Seq] = true
	}
	var extra []ChatMessage
	for _, m := range full {
		if !seen[m.Seq] {
			extra = append(extra, m)
		}
	}
	if len(extra) == 0 {
		return p
	}
	merged := make([]ChatMessage, 0, len(p.tr)+len(extra))
	ei := 0
	for _, m := range p.tr {
		// an unstamped row's place in the log is unknown: extras slide past
		// it only when a stamped row with a later seq names the spot
		for ei < len(extra) && m.Seq != NoSeq && extra[ei].Seq < m.Seq {
			merged = append(merged, extra[ei])
			ei++
		}
		merged = append(merged, m)
	}
	merged = append(merged, extra[ei:]...)
	return pickTr{tr: merged}
}

// pickerTranscriptLocked is pickerTranscript for a caller that holds ag.mu
// (a flush): the session snapshot and the stream state it already guards.
func (h *Host) pickerTranscriptLocked() pickTr {
	sess := h.Session()
	stream := h.streamLocked()
	if run := h.viewedRun(); run != nil {
		_, rows, _, _ := run.view(stream)
		return pickTr{tr: rows}
	}
	if !stream.Active || len(stream.Tools) == 0 {
		return pickTr{tr: sess.Transcript}
	}
	live := make([]ChatMessage, len(stream.Tools))
	for i, t := range stream.Tools {
		live[i] = ChatMessage{
			Role: fantasy.MessageRoleAssistant, Kind: RowTool, CallID: t.ID, Tool: tools.ToolName(t.Name),
			Text: t.Input, Label: t.Label, Status: t.Status, Detail: t.Detail,
			Job: t.Job, Run: t.Run, Diff: t.Diff, Output: t.Output,
		}
	}
	return pickTr{tr: sess.Transcript, live: live}
}

// OpenMessages lists the conversation where the reader is: the element
// holding their row, with the rail of the run holding it under the list when
// the reader sits in one. The row derives once, before anything opens, and
// the levels pin it (OpenAt), since the rail's open moves the chat.
func (h *Host) OpenMessages() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	p := h.withBrowseEchoes(h.pickerTranscript())
	row := ui.ChatRow()
	list := h.messagesList(p, row)
	list.OpenAt = row
	ui.OpenOverlay(list)
	ids := []string{"messages"}
	if run := messagesRunAt(p, row); run != 0 {
		if o, ok := h.messagesRailOverlay(p, run, row); ok {
			o.OpenAt = row
			ui.OpenOverlay(o)
			ids = append(ids, o.ID)
		}
	}
	h.trackMessages(ids...)
	return nil
}

// refreshMessages rebuilds the open levels of the messages picker from the
// conversation as it grows, so a call that settles or a reply that lands
// shows there; a level the conversation no longer holds (a reset, a
// compaction) closes. A caller holding ag.mu hands the conversation it read
// under that lock (flushOutput); the rest read it here.
func (h *Host) refreshMessages() {
	h.refreshMessagesFrom(h.pickerTranscript())
}

func (h *Host) refreshMessagesFrom(p pickTr) {
	h.refreshCall()
	ui := h.hooks()
	if ui == nil || h.viewedRun() != nil {
		return
	}
	h.msgMu.Lock()
	levels := slices.Clone(h.msgLevels)
	h.msgMu.Unlock()
	if len(levels) == 0 {
		return
	}
	var open []string
	for _, id := range levels {
		o, ok := h.messagesLevel(id, p)
		if !ok {
			ui.CloseOverlay(id, lua.OriginBuiltin, "")
			continue
		}
		open = append(open, id)
		ui.RefreshOverlay(id, func() Overlay { return o })
	}
	// a level the conversation dropped leaves the store; one a caller tracked
	// while the rebuild ran stays, or its first flush would never reach it
	h.msgMu.Lock()
	h.msgLevels = slices.DeleteFunc(h.msgLevels, func(id string) bool {
		return slices.Contains(levels, id) && !slices.Contains(open, id)
	})
	h.msgMu.Unlock()
}

// messagesLevel builds one of the picker's levels from the conversation, ok
// false for an id it no longer holds. A refresh keeps whatever the reader
// selected, so it names no row to open on.
func (h *Host) messagesLevel(id string, p pickTr) (Overlay, bool) {
	if id == "memories" {
		return h.memoriesOverlay(), true
	}
	if _, rest, ok := strings.Cut(id, " zoom "); ok { // a drill-in level
		if l, i, err := parseNodeArgs(rest); err == nil {
			return h.memoriesZoomOverlay(nodeID{l, i})
		}
		return Overlay{}, false
	}
	if _, rail, ok := strings.Cut(id, " rail "); ok { // a run's rail level
		if n, err := strconv.Atoi(rail); err == nil {
			return h.messagesRailOverlay(p, n, 0)
		}
		if seq, err := strconv.ParseInt(strings.TrimPrefix(rail, "#"), 10, 64); err == nil {
			return h.messagesRailOverlaySeq(seq)
		}
		return Overlay{}, false
	}
	if id == "messages" {
		return h.messagesList(p, 0), true
	}
	return Overlay{}, false
}

// trackMessages remembers the levels the picker opened, so they rebuild as the
// transcript grows. One copy each: every flush rebuilds them all.
func (h *Host) trackMessages(ids ...string) {
	h.msgMu.Lock()
	defer h.msgMu.Unlock()
	for _, id := range ids {
		if !slices.Contains(h.msgLevels, id) {
			h.msgLevels = append(h.msgLevels, id)
		}
	}
}

// messagesRunAt is the activity run holding a transcript row, 0 for none.
func messagesRunAt(p pickTr, row int) int {
	for _, t := range messagesTurns(p) {
		if !slices.Contains(t.rows, row) {
			continue
		}
		for _, e := range turnElements(p, t.rows) {
			if e.run != nil && slices.Contains(e.run.rows, row) {
				return e.run.first
			}
		}
	}
	return 0
}

// viewedTranscript is the chat's rows: the session's, or the shown run's,
// read the way a render reads them.
func (h *Host) viewedTranscript() []ChatMessage {
	sess, stream := h.liveView()
	if run := h.viewedRun(); run != nil {
		_, rows, _, _ := run.view(stream)
		return rows
	}
	return sess.Transcript
}

// msgTurn is one turn as /messages groups it: the conversation row it starts
// at (1-based, the row the chat previews) and every row it holds.
type msgTurn struct {
	n     int // 1-based, in order
	first int // the row the turn starts at
	rows  []int
}

// turnOpen reports whether a turn's rows hold neither a model's words nor
// an error: a user row landing in it is a steer the running call picked up,
// while one after a reply or an interrupt opens its own exchange.
func turnOpen(p pickTr, t *msgTurn) bool {
	for _, idx := range append([]int{t.first}, t.rows...) {
		r := p.at(idx)
		if r.Kind == RowError {
			return false
		}
		if r.Kind == "" && r.Role != fantasy.MessageRoleUser && strings.TrimSpace(r.Text) != "" {
			return false
		}
	}
	return true
}

// messagesTurns groups the conversation into turns: a user prompt opens one,
// and everything after it until the next prompt belongs to it. Rows before
// the first prompt (a resumed session's notes) join the first turn. The live
// turn's unsettled calls follow the transcript's rows as the negative rows
// they are anchored by.
func messagesTurns(p pickTr) []msgTurn {
	var turns []msgTurn
	// an attachment's or !command's echo logs before the message it rode, so
	// it holds for that message's turn; anything else arriving first pins it
	// to the turn before
	var pending []int
	add := func(idx int) {
		m := p.at(idx)
		if m.Kind == "" && m.Role == fantasy.MessageRoleUser && strings.TrimSpace(m.Text) != "" {
			// a user row that lands while the open turn holds neither reply
			// nor error is a steer the running call picked up, not a new
			// prompt: the exchange the chat draws is one
			if len(turns) > 0 && turnOpen(p, &turns[len(turns)-1]) {
				t := &turns[len(turns)-1]
				t.rows = append(t.rows, pending...)
				t.rows = append(t.rows, idx)
				pending = nil
				return
			}
			if len(pending) > 0 {
				turns = append(turns, msgTurn{n: len(turns) + 1, first: pending[0], rows: append(slices.Clone(pending), idx)})
				pending = nil
				return
			}
			turns = append(turns, msgTurn{n: len(turns) + 1, first: idx, rows: []int{idx}})
			return
		}
		if m.Kind == RowNotice && m.Tool != "" {
			pending = append(pending, idx)
			return
		}
		if len(turns) == 0 {
			turns = append(turns, msgTurn{n: 1, first: idx})
		} else {
			t := &turns[len(turns)-1]
			t.rows = append(t.rows, pending...)
			t.rows = append(t.rows, idx)
		}
		pending = nil
	}
	for idx := 1; idx <= len(p.tr); idx++ {
		add(idx)
	}
	for idx := -1; idx >= -len(p.live); idx-- {
		add(idx)
	}
	return turns
}

// messagesList is /messages: every turn's rows in order under a header that
// names the turn, so one list reads the whole conversation, with the pages a
// page-up loaded ahead of them and the logged rows this window never drew
// after them. The row the reader was at, when given, selects the element
// holding it.
func (h *Host) messagesList(p pickTr, at int) Overlay {
	o := Overlay{
		ID: "messages", Title: "Messages", Origin: lua.OriginBuiltin,
		Anchor: OverlayBottomRight, Follow: true, KeepOrder: true, Empty: "No messages yet.",
	}
	h.msgMu.Lock()
	old := slices.Clone(h.msgOld)
	from := h.msgFrom
	h.msgMu.Unlock()
	p.old = old
	// the loaded pages list the way the window's turns do: the same rows, a
	// run folded into the rail its row opens, all under the one Earlier
	// header, since none of them names a place in the chat window
	rows := make([]int, len(p.old))
	for i := range rows {
		rows[i] = -len(p.live) - 1 - i
	}
	// the cue leads the list while the log holds rows above the pages: a
	// move held on this row is what loads them (o.More), so the head's
	// earlier count and the first view no longer disagree in silence
	if h.DB != nil {
		if oldest, _ := h.messagesBound(from); oldest > 0 {
			o.Items = append(o.Items, OverlayItem{
				Text:    "… earlier messages above",
				Desc:    "up at this row loads them",
				Section: "Earlier",
			})
		}
	}
	// the wipe's own row leads the pages: the reset note the transcript
	// draws nothing of, reachable here and by view alone — the fold header
	// counts it
	if h.DB != nil {
		if first, err := h.DB.LogFrom(0, 1); err == nil && len(first) == 1 && first[0].Kind == store.KindNote {
			o.Items = append(o.Items, OverlayItem{
				Text:    firstLine(first[0].Text),
				Desc:    "a reset wiped the chat before this",
				Section: "Earlier",
				Run:     "messages view #" + strconv.FormatInt(first[0].Seq, 10),
				Verb:    VerbOpen,
			})
		}
	}
	for _, e := range turnElements(p, rows) {
		m := p.at(e.idx)
		var item OverlayItem
		if e.run != nil {
			item = railRow(*e.run)
			if first := p.at(e.run.first); first.Seq >= 0 {
				item.Run = "messages rail #" + strconv.FormatInt(first.Seq, 10)
				item.Verb = VerbOpen
				item.Anchor = 0
			}
		} else {
			item = olderItem(messageRow(e.idx, m), m)
		}
		if item.Text == "" {
			continue
		}
		item.Section = "Earlier"
		o.Items = append(o.Items, item)
	}
	var prevDay string
	for _, t := range messagesTurns(p) {
		section := turnSection(p, t, &prevDay)
		for _, e := range turnElements(p, t.rows) {
			var item OverlayItem
			if e.run != nil {
				item = railRow(*e.run)
			} else {
				item = messageRow(e.idx, p.at(e.idx))
			}
			if item.Text == "" {
				continue
			}
			if at > 0 && o.Selected == 0 {
				if e.run != nil && slices.Contains(e.run.rows, at) {
					o.Selected = len(o.Items)
				} else if e.run == nil && e.idx == at {
					o.Selected = len(o.Items)
				}
			}
			item.Section = section
			o.Items = append(o.Items, item)
		}
	}
	o.More = h.messagesOlder
	return o
}

// chatPageRows is how many log rows one page-up of the picker loads.
const chatPageRows = 250

// olderItem reshapes a windowed row's item for a loaded page row: the row
// names no place in the chat window, so enter opens rather than goes, and
// its views read the log row by seq. A call opens the view a live call's row
// opens; a run view reads the same wherever the row stands.
func olderItem(item OverlayItem, m ChatMessage) OverlayItem {
	if m.Seq <= NoSeq {
		return item
	}
	item.Anchor, item.Verb = 0, VerbOpen
	if m.Kind == RowTool || m.Kind == RowReasoning {
		if m.Run == "" {
			item.Run = "call #" + strconv.FormatInt(m.Seq, 10)
		}
		return item
	}
	if strings.HasPrefix(item.Run, "messages view ") {
		item.Run = "messages view #" + strconv.FormatInt(m.Seq, 10)
	}
	return item
}

// messagesBound names the seqs the browser's Earlier reaches back from:
// oldest is the loaded pages' from (else the window's oldest stamped row),
// head the window's oldest stamped row itself. Zero or less says the log's
// start is already on screen.
func (h *Host) messagesBound(from int64) (oldest, head int64) {
	// the window's oldest claimed row: the pages stop before it, whatever
	// seq it carries, the log's own 0 included
	h.tMu.RLock()
	tr, claimed := h.session.Transcript, h.stampRow
	h.tMu.RUnlock()
	head = NoSeq
	if claimed > 0 && len(tr) > 0 {
		head = tr[0].Seq
	} else {
		for _, m := range tr {
			if m.Seq != NoSeq {
				head = m.Seq
				break
			}
		}
	}
	switch {
	case from >= 0:
		return from, head
	case head < 0:
		return 0, head
	default:
		return head, head
	}
}

// messagesOlder loads the page of log rows before the oldest loaded one and
// returns the list rebuilt with it; ok false when nothing older is left. It
// runs on the UI worker, never on the key path.
func (h *Host) messagesOlder() (Overlay, bool) {
	if h.DB == nil {
		return Overlay{}, false
	}
	h.msgMu.Lock()
	from := h.msgFrom
	h.msgMu.Unlock()
	oldest, head := h.messagesBound(from)
	if oldest <= 0 {
		return Overlay{}, false
	}
	lo := max(int64(0), oldest-chatPageRows)
	if lo >= oldest {
		return Overlay{}, false
	}
	rows, err := h.DB.LogFrom(lo, int(oldest-lo))
	if err != nil || len(rows) == 0 {
		return Overlay{}, false
	}
	page := logRowsFor(rows, true)
	h.msgMu.Lock()
	// a reset wiped the log under the loaded pages: nothing survives it
	if head >= 0 {
		page = append(page, slices.DeleteFunc(h.msgOld, func(m ChatMessage) bool { return m.Seq >= head })...)
	}
	h.msgOld, h.msgFrom = page, lo
	h.msgMu.Unlock()
	return h.messagesList(h.pickerTranscript(), 0), true
}

// turnSection is the header a turn's rows sit under: its number, the clock
// of its prompt — dated when the day turns, so a days-later resume reads —
// and what it held after the prompt.
func turnSection(p pickTr, t msgTurn, prevDay *string) string {
	var desc []string
	// the turn's time is its prompt's: a notice (a goal echo) can lead the
	// rows in the spliced build, so the clock walks to the first row that
	// carries one instead of reading the head only
	for _, idx := range append([]int{t.first}, t.rows...) {
		if m := p.at(idx); m.SentAt > 0 {
			at := time.UnixMilli(m.SentAt)
			if day := at.Format("Jan 02"); day != *prevDay {
				desc = append(desc, day)
				*prevDay = day
			}
			desc = append(desc, at.Format(ClockLayout))
			break
		}
	}
	replies := 0
	// a turn's rows can be empty: a foreign reset mid-turn reloads the log
	// with the note dropped, so a transcript led by a tool row makes a turn
	// with no prompt row, and the refresh must read it, not die on it
	for _, idx := range t.rows[min(1, len(t.rows)):] {
		r := p.at(idx)
		// a reply is the model's words: the user's own echo (an attach) and
		// the user row itself do not count
		if r.Kind == "" && r.Role != fantasy.MessageRoleUser && strings.TrimSpace(r.Text) != "" {
			replies++
		}
	}
	if replies > 0 {
		desc = append(desc, pluralCount(replies, "reply", "replies"))
	}
	if len(desc) == 0 {
		return "Turn " + strconv.Itoa(t.n)
	}
	return "Turn " + strconv.Itoa(t.n) + " · " + strings.Join(desc, " · ")
}

// openMessagesRail lists the calls and thoughts of one activity run, the
// rail the list folds into a single row: the chat draws that run whole
// while this is up, and bands the call the selection names.
func (h *Host) openMessagesRail(first int) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	o, ok := h.messagesRailOverlay(h.pickerTranscript(), first, 0)
	if !ok {
		return Usagef("the chat shows no rail at row %d", first)
	}
	ui.OpenOverlay(o)
	h.trackMessages(o.ID)
	return nil
}

// openMessagesRailSeq lists one logged run call by call: the rail an Earlier
// row folds into, read from the log at the seq its row names.
func (h *Host) openMessagesRailSeq(seq int64) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	o, ok := h.messagesRailOverlaySeq(seq)
	if !ok {
		return Usagef("no run at message %d", seq)
	}
	ui.OpenOverlay(o)
	h.trackMessages(o.ID)
	return nil
}

// messagesRailOverlay is one activity run call by call: the rail the list
// folds into a row, whose run the chat draws whole while this is up.
// The row the reader was at, when given, selects the call holding it.
func (h *Host) messagesRailOverlay(p pickTr, first, at int) (Overlay, bool) {
	for _, t := range messagesTurns(p) {
		for _, e := range turnElements(p, t.rows) {
			if e.run == nil || e.run.first != first {
				continue
			}
			o := Overlay{
				// a level's id starts with the menu's root id, so the menu
				// closes whole from any of its levels
				ID:     "messages rail " + strconv.Itoa(first),
				Title:  "Messages / Turn " + strconv.Itoa(t.n) + " activity",
				Origin: lua.OriginBuiltin, Anchor: OverlayBottomRight, Follow: true, Rail: first, KeepOrder: true,
			}
			for _, idx := range e.run.rows {
				if at > 0 && o.Selected == 0 && idx == at {
					o.Selected = len(o.Items)
				}
				o.Items = append(o.Items, messageCallRow(idx, p.at(idx)))
			}
			return o, true
		}
	}
	return Overlay{}, false
}

// railLogScan is how many log rows one logged run's rail reads before it
// gives up on finding the row that ends it: a batch of parallel calls.
const railLogScan = 200

// messagesRailOverlaySeq is one logged run call by call: the rail an Earlier
// row folds into, read from the log at the seq its row names. The rows name
// no place in the chat window, so every one opens its view by seq.
func (h *Host) messagesRailOverlaySeq(seq int64) (Overlay, bool) {
	if h.DB == nil {
		return Overlay{}, false
	}
	rows, err := h.DB.LogFrom(seq, railLogScan)
	if err != nil || len(rows) == 0 || rows[0].Seq != seq {
		return Overlay{}, false
	}
	o := Overlay{
		ID:     "messages rail #" + strconv.FormatInt(seq, 10),
		Title:  "Messages / earlier activity",
		Origin: lua.OriginBuiltin, Anchor: OverlayBottomRight, Follow: true, KeepOrder: true,
	}
	for _, m := range logRows(rows) {
		if m.Kind != RowTool && m.Kind != RowReasoning {
			break // the run ended: what follows is a message of its own
		}
		o.Items = append(o.Items, olderItem(messageCallRow(0, m), m))
	}
	if len(o.Items) == 0 {
		return Overlay{}, false
	}
	return o, true
}

// msgElement is one row the list lists: a message, or the activity run
// its rail folds into.
type msgElement struct {
	idx int // the conversation row
	run *msgRun
}

// msgRun is a turn's activity run: the calls and thoughts the chat folds
// into one digest row, between the rows that are messages. The live turn's
// unsettled calls extend the run its settled rows end in.
type msgRun struct {
	first int   // the row it starts at
	rows  []int // every row it holds, the live calls negative
}

// railRow is a run as the list lists it: one row standing for the rail
// the chat draws folded, which l opens call by call. It reads the same
// whatever the run did, so the list keeps one shape.
func railRow(run msgRun) OverlayItem {
	text := pluralCount(len(run.rows), "step", "steps")
	return OverlayItem{
		Text: text, Anchor: run.first, Verb: VerbGo,
		Run: "messages rail " + strconv.Itoa(run.first), Row: []Segment{{Text: text}},
	}
}

// turnElements groups a turn's rows: a contiguous run of calls and thoughts
// is one element, the rail the chat draws folded; every other row is its own.
func turnElements(p pickTr, rows []int) []msgElement {
	activity := func(idx int) bool {
		k := p.at(idx).Kind
		return k == RowTool || k == RowReasoning
	}
	var out []msgElement
	for i := 0; i < len(rows); i++ {
		if !activity(rows[i]) {
			out = append(out, msgElement{idx: rows[i]})
			continue
		}
		j := i
		for j+1 < len(rows) && activity(rows[j+1]) {
			j++
		}
		out = append(out, msgElement{idx: rows[i], run: &msgRun{first: rows[i], rows: rows[i : j+1]}})
		i = j
	}
	return out
}

// messageCallRow is one call or thought of a rail: what it reads as, and the
// view or run its key opens over the list - a live call (a negative row)
// opens its live view, the one its chat row's click opens, which follows it
// until it settles.
func messageCallRow(idx int, m ChatMessage) OverlayItem {
	text := strings.TrimSpace(m.Text)
	item := OverlayItem{Anchor: idx}
	// a call with no label of its own names what it ran: "grep internal/ui"
	head := cmp.Or(m.Label, strings.TrimSpace(string(m.Tool)+" "+messageHead(text)), string(m.Tool))
	glyph, hl := "│", GroupDim
	if m.Kind == RowReasoning {
		head, glyph = cmp.Or(m.Label, messageHead(text), "thought"), "✻"
	}
	// enter is the row's place in the chat, l the view or run under it; the
	// job a call started opens from that view, so no key names it twice
	switch {
	case m.Run != "" && m.Tool == tools.ToolWorkflow:
		item.Run = "workflows view " + m.Run
	case m.Run != "":
		item.Run = "agents show " + m.Run
	default:
		item.Run = "call " + strconv.Itoa(idx)
	}
	if item.Run != "" {
		item.Verb = VerbGo
	}
	if n := lineCount(m.Output); m.Output != "" && n > 1 {
		item.Desc = pluralCount(n, "line", "lines")
	}
	// y copies the input the call acted on, not its raw text: the same
	// meaningful input its view leads with
	item.Copy = callInput(m)
	item.Text = head
	// a mark says what the row is, so the row draws no glyph of its own
	if mark := markFor(m); mark != "" {
		item.Mark, item.Row = mark, []Segment{{Text: head}}
		return item
	}
	item.Row = []Segment{{Text: glyph + " ", HL: hl}, {Text: head}}
	return item
}

// messageRow is one row of a turn: what it reads as, what y copies, and the
// actions that open it. Its text is empty for a row the list
// leaves out.
func messageRow(idx int, m ChatMessage) OverlayItem {
	text := strings.TrimSpace(m.Text)
	item := OverlayItem{Anchor: idx}
	var glyph, hl string
	var head, desc string
	switch {
	case m.Kind == "" && m.Role == fantasy.MessageRoleUser:
		head, glyph, hl = cmp.Or(messageHead(text), "(empty prompt)"), "▌", "UserMsg"
	case m.Kind == "" && text != "":
		blocks := codeBlocks(text)
		head, glyph, hl = messageHead(text), "◆", GroupDim
		if n := lineCount(text); n > 1 {
			desc = pluralCount(n, "line", "lines")
		}
		if len(blocks) > 0 {
			desc = strings.TrimSpace(desc + " · " + pluralCount(len(blocks), "code block", "code blocks"))
		}
	case m.Kind == RowNotice, m.Kind == RowError, m.Kind == RowShell:
		if text == "" {
			return OverlayItem{}
		}
		head, glyph = messageHead(text), "·"
		if m.Kind == RowError {
			glyph = "✗"
		}
	case m.Kind == RowTool, m.Kind == RowReasoning:
		return messageCallRow(idx, m)
	default:
		return OverlayItem{}
	}
	item.Text = head
	item.Desc = desc
	// y copies the row whole: an echo's rebuild carries its body in Detail
	// and only its head in Text
	if read := cmp.Or(strings.TrimSpace(m.Detail), text); read != "" {
		item.Copy = read
	}
	// a stamped row opens by its seq, the log's own identity: the splice
	// shifts indexes, the seq does not, so the view opens the row it names
	// whatever landed since the list was built
	if m.Seq >= 0 {
		item.Run, item.Verb = "messages view #"+strconv.FormatInt(m.Seq, 10), VerbGo
	} else {
		item.Run, item.Verb = "messages view "+strconv.Itoa(idx), VerbGo
	}
	if mark := markFor(m); mark != "" {
		item.Mark, item.Row = mark, []Segment{{Text: head}}
		return item
	}
	item.Row = []Segment{{Text: glyph + " ", HL: hl}, {Text: head}}
	return item
}

// markFor is the mark a row carries, so a failed call reads as one in the
// list as it does in the chat: the call status leads, so a cancelled turn's
// error row draws the chat's ■ and not the ✗ any error would.
func markFor(m ChatMessage) MarkKind {
	if k, ok := callMarks[m.Status]; ok {
		return k
	}
	if m.Kind == RowError {
		return tools.MarkErr
	}
	if m.Kind == RowNotice || m.Kind == RowShell {
		return tools.MarkNote
	}
	return MarkKind("")
}

// openMessageView reads one row whole: a reply as its markdown, a prompt or
// a notice as the text it is.
func (h *Host) openMessageView(n int) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	// the list's rows are numbered over the transcript its echoes splice
	// into (withBrowseEchoes): the same space here, or every echo past the
	// first shifts a later row's view onto its neighbor
	tr := h.withBrowseEchoes(h.pickerTranscript()).tr
	if n < 1 || n > len(tr) {
		return Usagef("the chat shows no row %d", n)
	}
	m := tr[n-1]
	// an echo's rebuild carries its body in Detail and only its head in Text
	text := cmp.Or(strings.TrimSpace(m.Detail), strings.TrimSpace(m.Text))
	if text == "" {
		return Usagef("row %d has no text", n)
	}
	title := "Message"
	md := false
	switch {
	case m.Kind == RowReasoning:
		title = "Thought"
		md = true
	case m.Kind == "" && m.Role == fantasy.MessageRoleUser:
		title, md = "Prompt", true
	case m.Kind == "":
		title = "Reply"
		md = true
	case m.Kind == RowNotice && m.Run != "":
		// a work report reads by its name, the way the log's row reads it back
		title = "Report"
	}
	ui.OpenOverlay(Overlay{
		ID: "messages view " + strconv.Itoa(n), Title: title, Origin: lua.OriginBuiltin,
		Body: text, BodyMD: md,
	})
	return nil
}

// openLogView reads one log row whole: the row a loaded page names by its
// seq, what "messages view <row>" is for the chat's window.
func (h *Host) openLogView(seq int64) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	row, err := h.DB.LogAt(seq)
	if err != nil {
		return Usagef("no message %d in the log", seq)
	}
	text := strings.TrimSpace(row.Text)
	if text == "" {
		return Usagef("message %d has no text", seq)
	}
	title, md := "Message", false
	switch row.Kind {
	case store.KindUser:
		title, md = "Prompt", true
	case store.KindAgent:
		title, md = "Reply", true
	case store.KindThought:
		title, md = "Thought", true
	case store.KindNote:
		title = "Note"
	case store.KindEcho:
		title = "Result"
	case store.KindWork:
		title = "Report"
	case store.KindTool:
		title = upperFirst(cmp.Or(rowTool(row), "Tool"))
	default: // an error or shell row reads by its text alone
	}
	ui.OpenOverlay(Overlay{
		ID: "messages view #" + strconv.FormatInt(seq, 10), Title: title, Origin: lua.OriginBuiltin,
		Body: text, BodyMD: md,
	})
	return nil
}

// messageHead is the line a message reads by: its first non-blank one, a
// fence line passed over so a reply that opens with a block reads as its
// code, a bare <zemi-note> wrapper passed over so a system note reads as its
// content; the first non-blank line when it is all wrappers.
func messageHead(text string) string {
	head := ""
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") || line == "<zemi-note>" {
			head = cmp.Or(head, line)
			continue
		}
		return line
	}
	return head
}

// pluralCount is "1 line", "2 code blocks".
func pluralCount(n int, one, many string) string {
	return fmt.Sprintf("%d %s", n, Plural(n, one, many))
}
