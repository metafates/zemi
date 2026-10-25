// messages.go is the messages picker: the conversation as one list, a row
// per message, with the chat scrolling to whatever the selection names. It
// is how an earlier prompt or reply is found and copied.

package hostapi

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
)

// messagesCommands is /messages: the conversation as one list, and the
// levels under it a list row opens.
func (h *Host) messagesCommands() []*Command {
	return []*Command{{
		Name: "messages", NArgs: NArgsAny, Desc: "the conversation as one list: read, copy or rewind a message",
		GoFn: func(h *Host, args string) error { return h.MessagesCommand(args) },
	}}
}

// MessagesCommand is /messages and the levels under it: no argument lists
// the conversation, "rail N" opens one run's rail, "view N" reads one row
// whole.
func (h *Host) MessagesCommand(args string) error {
	verb, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	switch verb {
	case "":
		return h.OpenMessages()
	case "rail", "view":
		n, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil {
			return Usagef("/messages %s <row> (a row the list shows)", verb)
		}
		if verb == "rail" {
			return h.openMessagesRail(n)
		}
		return h.openMessageView(n)
	}
	return Usagef("usage: /messages [rail <row> | view <row>]")
}

// pickTr is the conversation the picker reads: the session's transcript plus,
// while a turn runs, the calls it has not settled yet. The chat draws those
// on its live rail and marks them call = -i for the i-th of them, so a live
// call's anchor is negative and bands its own live rows. A run the transcript
// ends in is what they extend; until one call settles the picker shows the
// conversation without them.
type pickTr struct {
	tr   []ChatMessage
	live []ChatMessage
}

// at is the row idx names: 1.. for transcript rows, -1.. for the live calls.
func (p pickTr) at(idx int) ChatMessage {
	if idx > 0 {
		if idx <= len(p.tr) {
			return p.tr[idx-1]
		}
		return ChatMessage{}
	}
	if n := -idx; n <= len(p.live) {
		return p.live[n-1]
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

// pickerTranscriptLocked is pickerTranscript for a caller that holds ag.mu
// (a flush): the session snapshot and the stream state it already guards.
func (h *Host) pickerTranscriptLocked() pickTr {
	sess := h.Session()
	stream := h.streamLocked()
	if run := h.viewedRun(); run != nil {
		_, rows, _, _ := run.view(stream)
		return pickTr{tr: rows}
	}
	if !stream.Active || len(sess.Transcript) == 0 {
		return pickTr{tr: sess.Transcript}
	}
	if last := sess.Transcript[len(sess.Transcript)-1]; last.Kind != RowTool && last.Kind != RowReasoning {
		return pickTr{tr: sess.Transcript}
	}
	live := make([]ChatMessage, len(stream.Tools))
	for i, t := range stream.Tools {
		live[i] = ChatMessage{
			Role: fantasy.MessageRoleAssistant, Kind: RowTool, CallID: t.ID, Tool: t.Name,
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
	p := h.pickerTranscript()
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
// shows there; a level the conversation no longer holds (a rewind, a
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
			ui.CloseOverlay(id)
			continue
		}
		open = append(open, id)
		ui.RefreshOverlay(id, func() Overlay { return o })
	}
	h.msgMu.Lock()
	h.msgLevels = open
	h.msgMu.Unlock()
}

// messagesLevel builds one of the picker's levels from the conversation, ok
// false for an id it no longer holds. A refresh keeps whatever the reader
// selected, so it names no row to open on.
func (h *Host) messagesLevel(id string, p pickTr) (Overlay, bool) {
	if _, rail, ok := strings.Cut(id, " rail "); ok { // a run's rail level
		if n, err := strconv.Atoi(rail); err == nil {
			return h.messagesRailOverlay(p, n, 0)
		}
		return Overlay{}, false
	}
	if id == "messages" {
		return h.messagesList(p, 0), true
	}
	return Overlay{}, false
}

// trackMessages remembers the levels the picker opened, so they rebuild as the
// transcript grows.
func (h *Host) trackMessages(ids ...string) {
	h.msgMu.Lock()
	defer h.msgMu.Unlock()
	h.msgLevels = append(h.msgLevels, ids...)
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

// messagesTurns groups the conversation into turns: a user prompt opens one,
// and everything after it until the next prompt belongs to it. Rows before
// the first prompt (a resumed session's notes) join the first turn. The live
// turn's unsettled calls follow the transcript's rows as the negative rows
// they are anchored by.
func messagesTurns(p pickTr) []msgTurn {
	var turns []msgTurn
	add := func(idx int) {
		m := p.at(idx)
		if m.Kind == "" && m.Role == fantasy.MessageRoleUser && strings.TrimSpace(m.Text) != "" {
			turns = append(turns, msgTurn{n: len(turns) + 1, first: idx, rows: []int{idx}})
			return
		}
		if len(turns) == 0 {
			turns = append(turns, msgTurn{n: 1, first: idx})
		}
		t := &turns[len(turns)-1]
		t.rows = append(t.rows, idx)
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
// names the turn, so one list reads the whole conversation. The row the
// reader was at, when given, selects the element holding it.
func (h *Host) messagesList(p pickTr, at int) Overlay {
	o := Overlay{
		ID: "messages", Title: "Messages", Origin: lua.OriginBuiltin,
		Anchor: OverlayBottomRight, Follow: true, Empty: "No messages yet.",
	}
	for _, t := range messagesTurns(p) {
		section := turnSection(p, t)
		for _, e := range turnElements(p, t.rows) {
			var item OverlayItem
			if e.run != nil {
				item = railRow(*e.run)
			} else {
				item = h.messageRow(e.idx, p.at(e.idx))
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
			if item.ID != "" {
				o.Keys = h.rewindKey(o.Keys)
			}
		}
	}
	return o
}

// turnSection is the header a turn's rows sit under: its number, the clock
// of its prompt, and what it held after the prompt.
func turnSection(p pickTr, t msgTurn) string {
	var desc []string
	if m := p.at(t.first); m.SentAt > 0 {
		desc = append(desc, time.UnixMilli(m.SentAt).Format(ClockLayout))
	}
	replies, steps := 0, 0
	for _, idx := range t.rows[1:] {
		r := p.at(idx)
		switch {
		case r.Kind == "" && strings.TrimSpace(r.Text) != "":
			replies++
		case r.Kind == RowTool || r.Kind == RowReasoning:
			steps++
		}
	}
	if replies > 0 {
		desc = append(desc, pluralCount(replies, "reply", "replies"))
	}
	if steps > 0 {
		desc = append(desc, pluralCount(steps, "step", "steps"))
	}
	if len(desc) == 0 {
		return "Turn " + strconv.Itoa(t.n)
	}
	return "Turn " + strconv.Itoa(t.n) + " · " + strings.Join(desc, " · ")
}

// rewindKey adds r once: the key rewinds to the prompt a row names, and only
// such a row carries it.
func (h *Host) rewindKey(keys []OverlayKey) []OverlayKey {
	if slices.ContainsFunc(keys, func(k OverlayKey) bool { return k.Key == "r" }) {
		return keys
	}
	return append(keys, OverlayKey{
		Key: "r", Desc: "rewind", Danger: true,
		Applies: func(it OverlayItem) bool { return it.ID != "" },
		Act:     func(it OverlayItem) { h.rewindTo(it.ID) },
	})
}

// rewindTo runs the rewind command on a row's stored message, the way
// /rewind <id> does.
func (h *Host) rewindTo(id string) {
	if err := h.RunCommand(context.Background(), "rewind", id); err != nil {
		h.Notify(slog.LevelWarn, "Could not rewind", err.Error())
	}
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
				Origin: lua.OriginBuiltin, Anchor: OverlayBottomRight, Follow: true, Rail: first,
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
// view or run its key opens over the list. A live call (a negative row) is
// still running: it bands and copies but opens no view of its own.
func messageCallRow(idx int, m ChatMessage) OverlayItem {
	text := strings.TrimSpace(m.Text)
	item := OverlayItem{Anchor: idx}
	// a call with no label of its own names what it ran: "grep internal/ui"
	head := cmp.Or(m.Label, strings.TrimSpace(m.Tool+" "+messageHead(text)), m.Tool)
	glyph, hl := "│", "Dim"
	if m.Kind == RowReasoning {
		head, glyph = cmp.Or(m.Label, messageHead(text), "thought"), "✻"
	}
	// enter is the row's place in the chat, l the view or run under it; the
	// job a call started opens from that view, so no key names it twice
	switch {
	case m.Run != "" && m.Tool == "workflow":
		item.Run = "workflows view " + m.Run
	case m.Run != "":
		item.Run = "agents view " + m.Run
	case idx > 0:
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
// actions that open it or rewind to it. Its text is empty for a row the list
// leaves out.
func (h *Host) messageRow(idx int, m ChatMessage) OverlayItem {
	text := strings.TrimSpace(m.Text)
	item := OverlayItem{Anchor: idx}
	var glyph, hl string
	var head, desc string
	switch {
	case m.Kind == "" && m.Role == fantasy.MessageRoleUser:
		head, glyph, hl = cmp.Or(messageHead(text), "(empty prompt)"), "▌", "UserMsg"
		if m.ID != "" {
			item.ID = m.ID // r rewinds to it; the list's key checks the rows
		}
	case m.Kind == "" && text != "":
		blocks := codeBlocks(text)
		head, glyph, hl = messageHead(text), "◆", "Dim"
		if n := lineCount(text); n > 1 {
			desc = pluralCount(n, "line", "lines")
		}
		if len(blocks) > 0 {
			desc = strings.TrimSpace(desc + " · " + pluralCount(len(blocks), "code block", "code blocks"))
		}
	case m.Kind == RowNotice, m.Kind == RowCompact, m.Kind == RowError, m.Kind == RowShell:
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
	if text != "" {
		item.Copy = text
	}
	item.Run, item.Verb = "messages view "+strconv.Itoa(idx), VerbGo
	if mark := markFor(m); mark != "" {
		item.Mark, item.Row = mark, []Segment{{Text: head}}
		return item
	}
	item.Row = []Segment{{Text: glyph + " ", HL: hl}, {Text: head}}
	return item
}

// markFor is the mark a row carries, so a failed call reads as one in the
// list as it does in the chat.
func markFor(m ChatMessage) MarkKind {
	if m.Kind == RowError {
		return MarkErr
	}
	if m.Kind == RowNotice || m.Kind == RowCompact || m.Kind == RowShell {
		return MarkNote
	}
	if k, ok := callMarks[m.Status]; ok {
		return k
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
	tr := h.viewedTranscript()
	if n < 1 || n > len(tr) {
		return Usagef("the chat shows no row %d", n)
	}
	m := tr[n-1]
	text := strings.TrimSpace(m.Text)
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
	}
	ui.OpenOverlay(Overlay{
		ID: "messages view " + strconv.Itoa(n), Title: title, Origin: lua.OriginBuiltin,
		Body: text, BodyMD: md,
	})
	return nil
}

// messageHead is the line a message reads by: its first non-blank one, a
// fence line passed over so a reply that opens with a block reads as its
// code; the first non-blank line when it is all fences.
func messageHead(text string) string {
	head := ""
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
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
