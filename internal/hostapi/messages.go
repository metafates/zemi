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

// messagesCommands is /messages: the conversation as turns, and the levels
// under it a list row opens.
func (h *Host) messagesCommands() []*Command {
	return []*Command{{
		Name: "messages", NArgs: NArgsAny, Desc: "the conversation as turns: read, copy or rewind a message",
		GoFn: func(h *Host, args string) error { return h.MessagesCommand(args) },
	}}
}

// MessagesCommand is /messages and the levels under it: no argument lists the
// turns, "turn N" opens one turn's messages and rail, "view N" reads one row
// whole.
func (h *Host) MessagesCommand(args string) error {
	verb, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	switch verb {
	case "":
		return h.OpenMessages()
	case "turn", "rail", "view":
		n, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil {
			return Usagef("/messages %s <row> (a row the list shows)", verb)
		}
		switch verb {
		case "turn":
			return h.openMessagesTurn(n)
		case "rail":
			return h.openMessagesRail(n)
		}
		return h.openMessageView(n)
	}
	return Usagef("usage: /messages [turn <row> | rail <row> | view <row>]")
}

// OpenMessages lists the conversation where the reader is: the deepest level
// holding their place, the way muffin opens on the symbol under the cursor.
// A row the chat draws from no turn opens the list of turns.
func (h *Host) OpenMessages() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	tr := h.viewedTranscript()
	ui.OpenOverlay(h.messagesOverlay(tr))
	first, run := messagesAt(tr, ui.ChatRow())
	if first == 0 {
		h.trackMessages("messages")
		return nil
	}
	ids := []string{"messages", "messages turn " + strconv.Itoa(first)}
	if o, ok := h.messagesTurnOverlay(tr, first); ok {
		ui.OpenOverlay(o)
	}
	if run != 0 {
		if o, ok := h.messagesRailOverlay(tr, run); ok {
			ui.OpenOverlay(o)
			ids = append(ids, o.ID)
		}
	}
	h.trackMessages(ids...)
	return nil
}

// refreshMessages rebuilds the open levels of the messages picker from the
// transcript as it grows, so a call that settles or a reply that lands shows
// there; a level the transcript no longer holds (a rewind, a compaction)
// closes. It reads the session, never the agent's stream: a caller may hold
// that lock.
func (h *Host) refreshMessages() {
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
	tr := h.Session().Transcript
	var open []string
	for _, id := range levels {
		o, ok := h.messagesLevel(id, tr)
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

// messagesLevel builds one of the picker's levels from a transcript, ok false
// for an id it no longer holds.
func (h *Host) messagesLevel(id string, tr []ChatMessage) (Overlay, bool) {
	if _, rail, ok := strings.Cut(id, " rail "); ok { // a turn's rail level
		if n, err := strconv.Atoi(rail); err == nil {
			return h.messagesRailOverlay(tr, n)
		}
		return Overlay{}, false
	}
	switch {
	case id == "messages":
		return h.messagesOverlay(tr), true
	case strings.HasPrefix(id, "messages turn "):
		if n, err := strconv.Atoi(strings.TrimPrefix(id, "messages turn ")); err == nil {
			return h.messagesTurnOverlay(tr, n)
		}
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

// messagesAt is the level of /messages that holds a transcript row: the turn
// it belongs to, and the activity run of that turn holding it, 0 for none.
func messagesAt(tr []ChatMessage, row int) (first, run int) {
	for _, t := range messagesTurns(tr) {
		if !slices.Contains(t.rows, row) {
			continue
		}
		for _, e := range turnElements(tr, t.rows) {
			if e.run != nil && slices.Contains(e.run.rows, row) {
				run = e.run.first
			}
		}
		return t.first, run
	}
	return 0, 0
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

// msgTurn is one turn as /messages groups it: the transcript row it starts
// at (1-based, the row the chat previews) and every row it holds.
type msgTurn struct {
	n     int // 1-based, in order
	first int // the transcript row the turn starts at
	rows  []int
}

// messagesTurns groups the transcript into turns: a user prompt opens one,
// and everything after it until the next prompt belongs to it. Rows before
// the first prompt (a resumed session's notes) join the first turn.
func messagesTurns(tr []ChatMessage) []msgTurn {
	var turns []msgTurn
	for i, m := range tr {
		idx := i + 1
		if m.Kind == "" && m.Role == fantasy.MessageRoleUser && strings.TrimSpace(m.Text) != "" {
			turns = append(turns, msgTurn{n: len(turns) + 1, first: idx, rows: []int{idx}})
			continue
		}
		if len(turns) == 0 {
			turns = append(turns, msgTurn{n: 1, first: idx})
		}
		t := &turns[len(turns)-1]
		t.rows = append(t.rows, idx)
	}
	return turns
}

// messagesOverlay is /messages: one row per turn, the prompt it opens on
// first, and what it held after it.
func (h *Host) messagesOverlay(tr []ChatMessage) Overlay {
	o := Overlay{
		ID: "messages", Title: "Messages", Origin: lua.OriginBuiltin,
		Anchor: OverlayBottomRight, Follow: true, Empty: "No messages yet.",
	}
	for _, t := range messagesTurns(tr) {
		m := tr[t.first-1]
		text := strings.TrimSpace(m.Text)
		var desc []string
		if m.SentAt > 0 {
			desc = append(desc, time.UnixMilli(m.SentAt).Format(ClockLayout))
		}
		replies, steps := 0, 0
		for _, idx := range t.rows[1:] {
			r := tr[idx-1]
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
		head := clipRunes(cmp.Or(messageHead(text), "(untitled turn)"), 60)
		item := OverlayItem{
			Text: head, Desc: strings.Join(desc, " · "), Anchor: t.first,
			Verb: VerbGo, Run: "messages turn " + strconv.Itoa(t.first),
			Row: []Segment{{Text: "▌ ", HL: "UserMsg"}, {Text: head}},
		}
		if text != "" {
			item.Copy = text
		}
		if m.ID != "" {
			item.ID = m.ID
			o.Keys = h.rewindKey(o.Keys)
		}
		o.Items = append(o.Items, item)
	}
	return o
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

// openMessagesTurn lists what one turn held: its prompt, replies, notices,
// commands and errors, then the rail of calls and runs a click opens. Every
// row names its transcript row, so the chat follows the selection here too.
func (h *Host) openMessagesTurn(first int) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	o, ok := h.messagesTurnOverlay(h.viewedTranscript(), first)
	if !ok {
		return Usagef("the chat shows no turn at row %d", first)
	}
	ui.OpenOverlay(o)
	h.trackMessages(o.ID)
	return nil
}

// messagesTurnOverlay is one turn's rows: its prompt, replies, notices,
// commands and errors, and its activity run folded into the row l opens.
func (h *Host) messagesTurnOverlay(tr []ChatMessage, first int) (Overlay, bool) {
	var turn msgTurn
	for _, t := range messagesTurns(tr) {
		if t.first == first {
			turn = t
		}
	}
	if turn.first != first {
		return Overlay{}, false
	}
	o := Overlay{
		ID: "messages turn " + strconv.Itoa(first), Title: "Messages / Turn " + strconv.Itoa(turn.n),
		Origin: lua.OriginBuiltin, Anchor: OverlayBottomRight, Follow: true,
		Empty: "This turn held nothing.",
	}
	for _, e := range turnElements(tr, turn.rows) {
		var item OverlayItem
		if e.run != nil {
			item = railRow(*e.run)
		} else {
			item = h.messageRow(e.idx, tr[e.idx-1])
		}
		if item.Text == "" {
			continue
		}
		o.Items = append(o.Items, item)
		if item.ID != "" {
			o.Keys = h.rewindKey(o.Keys)
		}
	}
	return o, true
}

// openMessagesRail lists the calls and thoughts of one activity run, the
// rail the turn view folds into a single row: the chat draws that run whole
// while this is up, and bands the call the selection names.
func (h *Host) openMessagesRail(first int) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	o, ok := h.messagesRailOverlay(h.viewedTranscript(), first)
	if !ok {
		return Usagef("the chat shows no rail at row %d", first)
	}
	ui.OpenOverlay(o)
	h.trackMessages(o.ID)
	return nil
}

// messagesRailOverlay is one activity run call by call: the rail the turn
// view folds into a row, whose run the chat draws whole while this is up.
func (h *Host) messagesRailOverlay(tr []ChatMessage, first int) (Overlay, bool) {
	for _, t := range messagesTurns(tr) {
		for _, e := range turnElements(tr, t.rows) {
			if e.run == nil || e.run.first != first {
				continue
			}
			o := Overlay{
				// a level's id starts with the id of the one it opened from,
				// so the menu closes whole from any of its levels
				ID:     "messages turn " + strconv.Itoa(t.first) + " rail " + strconv.Itoa(first),
				Title:  "Messages / Turn " + strconv.Itoa(t.n) + " activity",
				Origin: lua.OriginBuiltin, Anchor: OverlayBottomRight, Follow: true, Rail: first,
			}
			for _, idx := range e.run.rows {
				o.Items = append(o.Items, messageCallRow(idx, tr[idx-1]))
			}
			return o, true
		}
	}
	return Overlay{}, false
}

// msgElement is one row the turn view lists: a message, or the activity run
// its rail folds into.
type msgElement struct {
	idx int // the transcript row
	run *msgRun
}

// msgRun is a turn's activity run: the calls and thoughts the chat folds
// into one digest row, between the rows that are messages.
type msgRun struct {
	first int   // the transcript row it starts at
	rows  []int // every row it holds
}

// railRow is a run as the turn view lists it: one row standing for the rail
// the chat draws folded, which l opens call by call. It reads the same
// whatever the run did, so a list of turns keeps one shape.
func railRow(run msgRun) OverlayItem {
	text := pluralCount(len(run.rows), "step", "steps")
	return OverlayItem{
		Text: text, Anchor: run.first, Verb: VerbGo,
		Run: "messages rail " + strconv.Itoa(run.first), Row: []Segment{{Text: text}},
	}
}

// turnElements groups a turn's rows: a contiguous run of calls and thoughts
// is one element, the rail the chat draws folded; every other row is its own.
func turnElements(tr []ChatMessage, rows []int) []msgElement {
	activity := func(idx int) bool {
		k := tr[idx-1].Kind
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
// view or run its key opens over the list.
func messageCallRow(idx int, m ChatMessage) OverlayItem {
	text := strings.TrimSpace(m.Text)
	item := OverlayItem{Anchor: idx}
	// a call with no label of its own names what it ran: "grep internal/ui"
	head := clipRunes(cmp.Or(m.Label, strings.TrimSpace(m.Tool+" "+messageHead(text)), m.Tool), 40)
	glyph, hl := "│", "Dim"
	if m.Kind == RowReasoning {
		head, glyph = clipRunes(cmp.Or(m.Label, messageHead(text), "thought"), 40), "✻"
	}
	// enter is the row's place in the chat, l the view or run under it; the
	// job a call started opens from that view, so no key names it twice
	switch {
	case m.Run != "" && m.Tool == "workflow":
		item.Run = "workflows view " + m.Run
	case m.Run != "":
		item.Run = "agents view " + m.Run
	default:
		item.Run = "call " + strconv.Itoa(idx)
	}
	item.Verb = VerbGo
	if n := lineCount(m.Output); m.Output != "" && n > 1 {
		item.Desc = pluralCount(n, "line", "lines")
	}
	if text != "" {
		item.Copy = text
	}
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
		head, glyph, hl = clipRunes(cmp.Or(messageHead(text), "(empty prompt)"), 40), "▌", "UserMsg"
		if m.ID != "" {
			item.ID = m.ID // r rewinds to it; the list's key checks the rows
		}
	case m.Kind == "" && text != "":
		blocks := codeBlocks(text)
		head, glyph, hl = clipRunes(messageHead(text), 40), "◆", "Dim"
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
		head, glyph = clipRunes(messageHead(text), 40), "·"
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
