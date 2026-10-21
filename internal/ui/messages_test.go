package ui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
)

// TestUnitAt: a row whose own anchor is not drawn belongs to the nearest unit
// before it (a notice folded into its burst, a call inside a run's digest),
// and a row above every unit belongs to none.
func TestUnitAt(t *testing.T) {
	lines := [][]hostapi.Segment{
		{{Text: ""}},
		{{Text: "a", Anchor: 1}},
		{{Text: "a", Anchor: 1}}, // a wrapped row carries it again
		{{Text: ""}},
		{{Text: "b", Anchor: 4}},
	}
	for _, c := range []struct{ at, want int }{
		{1, 1}, {2, 1}, {3, 1}, {4, 4}, {5, 4}, {0, 0},
	} {
		if got := unitAt(lines, c.at); got != c.want {
			t.Errorf("anchor %d: unit %d, want %d", c.at, got, c.want)
		}
	}
	if got := unitAt(nil, 3); got != 0 {
		t.Fatalf("nothing drawn: unit %d", got)
	}
}

// banded reports whether a composed row carries the picker's band.
func banded(s hostapi.Segment) bool { return s.Fill == hostapi.FillBand }

// bandedChatRows is the text of the chat rows the picker's message is banded
// on, as the last frame drew them.
func bandedChatRows(app *App) []string {
	chat := app.scrolls["chat"]
	var out []string
	for y := chat.rect.Min.Y; y < min(chat.rect.Max.Y, len(app.lastRows)); y++ {
		// the chat's first cell, so a box drawn over the rows is no band here
		if row := dropCells(app.lastRows[y], chat.rect.Min.X); len(row) > 0 && banded(row[0]) {
			out = append(out, strings.TrimSpace(hostapi.SegsText(app.lastRows[y])))
		}
	}
	return out
}

// fillOneTurn fills one turn: its prompt and 30 replies, so a turn's own
// rows are a long conversation, each one a message the chat draws.
func fillOneTurn(host *hostapi.Host) {
	host.AppendMessage("user", "message 1")
	for i := 1; i <= 30; i++ {
		appendReply(host, fmt.Sprintf("reply %d", i), hostapi.TurnStat{In: 1000, Out: 100, ElapsedMS: 1500})
	}
}

// openTurnRows opens the one turn of fillOneTurn as its rows: the level a
// message is picked at, where every row names its own transcript row.
func openTurnRows(t *testing.T, app *App, host *hostapi.Host) *overlayState {
	t.Helper()
	if err := host.MessagesCommand("turn 1"); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	if len(app.overlays) != 1 || !app.overlays[0].o.Follow {
		t.Fatalf("overlays: %+v", app.overlays)
	}
	return &app.overlays[0]
}

// TestMessagesOpenAtGap: with the chat at its top, or with the view's first
// row the blank line between two messages, the picker opens on the message
// the reader sees, never on the one already scrolled out of view above.
func TestMessagesOpenAtGap(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	open := func() {
		t.Helper()
		if err := host.OpenMessages(); err != nil {
			t.Fatal(err)
		}
		for app.Pump() {
		}
		if len(app.overlays) == 0 {
			t.Fatal("no overlays")
		}
		from, to := unitRows(chat.lines, app.overlays[len(app.overlays)-1].selectedAnchor())
		if from < chat.top || from >= chat.top+chat.viewH {
			t.Fatalf("the picked message is off screen: rows %d..%d, view %d..%d", from, to, chat.top, chat.top+chat.viewH)
		}
		press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
		app.Frame()
	}

	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	app.Frame()
	if chat.top != 0 {
		t.Fatalf("not at the top: %d", chat.top)
	}
	open()

	// a blank row between two messages at the view's top, the message above
	// it ends before the view starts
	delete(app.pendingScroll, "chat")
	for top := chat.rows - chat.viewH; top > 0; top-- {
		chat.top = top
		if rowAnchor(chat.lines[top]) == 0 && rowAnchor(chat.lines[top+1]) > 0 {
			break
		}
	}
	app.Frame()
	if chat.top == 0 || rowAnchor(chat.lines[chat.top]) != 0 || rowAnchor(chat.lines[chat.top+1]) == 0 {
		t.Fatalf("no gap at the top: top %d", chat.top)
	}
	open()
}

// TestMessagesOpenKeepsThePick: the levels of the picker open on the message
// the reader sees and leave the chat there. A jump the turns list asked for
// (to a turn's first row) must not outlive the turn level whose own row is in
// view: the chat would land on the prompt, and the next open would pick it.
func TestMessagesOpenKeepsThePick(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 16)
	fillOneTurn(host)
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	pump := func() {
		for range 3 {
			for app.Pump() {
			}
			app.Frame()
			app.renderSync(t)
		}
		app.Frame()
	}
	for range 4 {
		press(t, app, tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
	}
	pump()
	if chat.top == 0 {
		t.Fatalf("the chat did not scroll: top %d", chat.top)
	}
	for n := range 3 {
		if err := host.OpenMessages(); err != nil {
			t.Fatal(err)
		}
		pump()
		top := &app.overlays[len(app.overlays)-1]
		pick := top.selectedAnchor()
		if from, to := unitRows(chat.lines, pick); from < 0 || from < chat.top || to >= chat.top+chat.viewH {
			t.Fatalf("open %d: the picked row %d is off screen, rows %d..%d of view %d..%d", n, pick, from, to, chat.top, chat.top+chat.viewH)
		}
		if pick == 1 {
			t.Fatalf("open %d landed on the turn's prompt with the chat at row %d", n, app.readRow)
		}
		press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
		pump()
		press(t, app, tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
		pump()
	}
}

// TestMessagesOpenOnRun: a run of calls is no message the list names, so a
// view whose top row is the run's digest hands over to the message under it,
// never back to the reply above, which may be many rows away.
func TestMessagesOpenOnRun(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 12)
	host.AppendMessage("user", "one")
	var long strings.Builder
	for i := range 20 {
		fmt.Fprintf(&long, "line %d\n\n", i)
	}
	appendReply(host, long.String(), hostapi.TurnStat{})
	for _, tool := range []struct{ name, text, label string }{
		{"read", "a.go", "read a"}, {"glob", "*.go", "find files"}, {"shell", "go test", "run the tests"},
	} {
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: tool.name, Text: tool.text, Label: tool.label, Status: "ok"})
	}
	appendReply(host, "reply two", hostapi.TurnStat{})
	// a long tail, so the run's digest can stand at the view's top
	host.AppendMessage("user", "three")
	appendReply(host, long.String(), hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	digest := -1
	for i, row := range chat.lines {
		if strings.HasPrefix(hostapi.SegsText(row), "· ") {
			digest = i
		}
	}
	if digest < 0 || chat.rows <= chat.viewH {
		t.Fatalf("no digest row to scroll to, or the chat does not scroll: %d, %d rows", digest, chat.rows)
	}
	delete(app.pendingScroll, "chat")
	chat.top, chat.focus, chat.follow = digest, 0, false
	app.Frame()
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	ov := &app.overlays[0]
	if got := ov.items[ov.visible[ov.selected]].label; !strings.Contains(got, "one") {
		t.Fatalf("opened on %q, want the turn the run sits in", got)
	}
}

// TestMessagesBandKeepsScrollbar: the scrollbar is not part of the message a
// picker bands, so its own cell keeps the canvas under the band.
func TestMessagesBandKeepsScrollbar(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	host.AppendMessage("user", "long one")
	var b strings.Builder
	for i := range 40 {
		fmt.Fprintf(&b, "line %d\n\n", i)
	}
	appendReply(host, b.String(), hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome})
	app.Frame()
	chat := app.scrolls["chat"]
	if !chat.overflows() {
		t.Fatal("the chat should scroll")
	}
	from, _ := unitRows(chat.lines, app.overlays[0].selectedAnchor())
	cell := dropCells(app.lastRows[chat.rect.Min.Y+from-chat.top], chat.rect.Max.X-1)[0]
	if !cell.Bar || cell.Fill == hostapi.FillBand {
		t.Fatalf("the scrollbar took the band: %+v", cell)
	}
}

// TestMessagesTallJump: a message taller than the view starts just under the
// pinned header above it, instead of being drawn over by it.
func TestMessagesTallJump(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	// the turn's prompt is the row a turn is picked by, so a tall turn is a
	// long prompt
	var b strings.Builder
	for i := range 40 {
		fmt.Fprintf(&b, "line %d\n\n", i)
	}
	host.AppendMessage("user", "long one\n\n"+b.String())
	appendReply(host, "done", hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	press(t, app, tea.KeyPressMsg{Code: 'h', Text: "h"}) // back to the turns
	app.Frame()
	chat := app.scrolls["chat"]
	ov := &app.overlays[len(app.overlays)-1]
	// away and back, so the jump to this turn is what the second key makes
	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome})
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnd})
	app.Frame()
	from, to := unitRows(chat.lines, ov.selectedAnchor())
	if from < 0 || to-from < chat.viewH {
		t.Fatalf("the picked message is not taller than the view: %d..%d", from, to)
	}
	pins := len(pinned(nil, chat.lines, chat.top))
	if from != chat.top+pins {
		t.Fatalf("the message starts at view row %d, %d pins over it", from-chat.top, pins)
	}
}

// TestMessagesFollow: the messages picker opens on the row the reader is at,
// moving its selection brings that message's first row to the top of the
// chat and bands it, and a row in view at the tail leaves the chat following
// it.
func TestMessagesFollow(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	fillOneTurn(host)
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	if chat == nil || !chat.follow {
		t.Fatal("the chat should follow its tail")
	}
	open := func() *overlayState {
		t.Helper()
		if err := host.MessagesCommand("turn 1"); err != nil {
			t.Fatal(err)
		}
		for app.Pump() {
		}
		return openTurnRows(t, app, host)
	}
	ov := open()
	if got := ov.selectedAnchor(); got != 31 { // the last reply, at the tail
		t.Fatalf("opened on anchor %d", got)
	}
	if top := chat.top; top != chat.rows-chat.viewH {
		t.Fatalf("opening moved the chat: top %d of %d, view %d", top, chat.rows, chat.viewH)
	}

	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome})
	app.Frame()
	if chat.follow {
		t.Fatal("a selection above the tail should leave it")
	}
	if chat.top != 0 { // the whole chat is under the first message already
		t.Fatalf("the first message should keep top 0, got %d", chat.top)
	}
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "message 1") {
		t.Fatalf("banded rows %q", got)
	}

	// a message well inside the chat lands in the middle of the view
	ov.selected = 21 // reply 21
	app.Frame()
	from, to := unitRows(chat.lines, ov.selectedAnchor())
	if from < chat.top || to >= chat.top+chat.viewH {
		t.Fatalf("the message is off screen: rows %d..%d, view %d..%d", from, to, chat.top, chat.top+chat.viewH)
	}
	if mid := (chat.viewH - (to - from + 1)) / 2; from-chat.top != mid {
		t.Fatalf("the message starts at view row %d, want it centered at %d", from-chat.top, mid)
	}
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "reply 21") {
		t.Fatalf("banded rows %q", got)
	}

	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnd})
	app.Frame()
	if !chat.follow || chat.top != chat.rows-chat.viewH {
		t.Fatal("the last row should hand the chat back to its tail")
	}
	// the readout under the reply is meta: never banded
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "reply 30") || strings.Contains(got[0], "out") {
		t.Fatalf("banded rows %q", got)
	}
	// a row already in the tail's view keeps the chat on the tail
	press(t, app, tea.KeyPressMsg{Code: 'k', Text: "k"})
	app.Frame()
	if !chat.follow {
		t.Fatal("a row in view at the tail should keep following it")
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	app.Frame()
	if !chat.follow || chat.top != chat.rows-chat.viewH {
		t.Fatalf("esc should put a following chat back on its tail: top %d of %d", chat.top, chat.rows)
	}

	// scrolled up, the picker opens on the message the top row belongs to and
	// esc puts the chat back where it was
	for range 3 {
		press(t, app, tea.KeyPressMsg{Code: tea.KeyPgUp})
	}
	app.Frame()
	top := chat.top
	ov = open()
	from, to = unitRows(chat.lines, ov.selectedAnchor())
	if from < 0 || from >= chat.top+chat.viewH || to < chat.top {
		t.Fatalf("opened on an off-screen message: rows %d..%d, view %d..%d", from, to, chat.top, chat.top+chat.viewH)
	}
	app.Frame()
	if chat.top != top {
		t.Fatalf("opening moved the chat: %d -> %d", top, chat.top)
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome})
	app.Frame()
	if chat.top == top {
		t.Fatal("a jump should move the chat")
	}
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "message 1") {
		t.Fatalf("banded rows %q", got)
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	app.Frame()
	if chat.top != top || len(app.overlays) != 0 {
		t.Fatalf("esc should cancel: top %d, want %d, %d overlays", chat.top, top, len(app.overlays))
	}
	if got := bandedChatRows(app); len(got) != 0 {
		t.Fatalf("banded rows after esc: %q", got)
	}

	// enter keeps the place instead
	open()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome})
	app.Frame()
	jumped := chat.top
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
	app.Frame()
	if len(app.overlays) != 0 {
		t.Fatalf("%d overlays after enter", len(app.overlays))
	}
	if chat.top != jumped || chat.top == top {
		t.Fatalf("enter should keep the jump: top %d, jumped %d, was %d", chat.top, jumped, top)
	}
	if got := bandedChatRows(app); len(got) != 0 {
		t.Fatalf("banded rows after enter: %q", got)
	}
}

// TestMessagesOpensAtTheReader: /messages opens on the level the reader is
// in, not on the list of turns, and enter there closes the menu keeping the
// place it moved the chat to.
func TestMessagesOpensAtTheReader(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 120, 46)
	fillChat(host)
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	open := func() *overlayState {
		t.Helper()
		if err := host.OpenMessages(); err != nil {
			t.Fatal(err)
		}
		for app.Pump() {
		}
		if len(app.overlays) < 2 {
			t.Fatalf("%d overlays, want the turns and the one level in", len(app.overlays))
		}
		return &app.overlays[len(app.overlays)-1]
	}
	// every turn's prompt, put at the top of the view, opens its own level
	turns := 0
	for _, line := range chat.lines {
		anchor := rowAnchor(line)
		if !strings.Contains(hostapi.SegsText(line), "message ") || anchor == 0 {
			continue
		}
		var from int
		for i, l := range chat.lines {
			if rowAnchor(l) == anchor {
				from = i
				break
			}
		}
		chat.follow, chat.top, chat.focus = false, from, 0
		app.Frame()
		ov := open()
		if !strings.HasPrefix(ov.o.ID, "messages turn ") {
			t.Fatalf("the reader at %d opened %q", anchor, ov.o.ID)
		}
		if got := ov.selectedAnchor(); got != anchor && !strings.Contains(fmt.Sprint(ov.labels), "message") {
			t.Fatalf("the reader at %d opened on %d", anchor, got)
		}
		want := ov.selectedAnchor()
		top := chat.top
		press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
		app.Frame()
		if len(app.overlays) != 0 {
			t.Fatalf("enter left %d overlays", len(app.overlays))
		}
		if chat.top != top {
			t.Fatalf("enter moved the chat: top %d, want %d", chat.top, top)
		}
		// reopening lands on the same row again
		if got := open().selectedAnchor(); got != want {
			t.Fatalf("reopened on %d, want %d", got, want)
		}
		press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
		app.Frame()
		turns++
	}
	if turns < 5 {
		t.Fatalf("only %d turns read", turns)
	}
}

// TestMessagesBottomDropsPlace: scrolling to the bottom drops where a jump
// put the view, even when the chat is already pinned to its tail and the
// scroll moves nothing: the picker then opens on the last message.
func TestMessagesBottomDropsPlace(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 20)
	fillOneTurn(host)
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	open := func() *overlayState {
		t.Helper()
		if err := host.OpenMessages(); err != nil {
			t.Fatal(err)
		}
		for app.Pump() {
		}
		return &app.overlays[0]
	}
	// a turn's own rows: a jump there, then enter keeps that place
	rows := openTurnRows(t, app, host)
	rows.selected = 21 // reply 21
	app.Frame()
	if chat.focus != 22 {
		t.Fatalf("follow %v, focus %d", chat.follow, chat.focus)
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
	app.Frame()
	// the chat is already at its tail: this scroll moves nothing
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
	app.Frame()
	if chat.focus != 0 {
		t.Fatalf("a scroll to the bottom kept the place: focus %d", chat.focus)
	}
	if got := open().selectedAnchor(); got != 1 {
		t.Fatalf("opened on %d, want the turn's prompt 1", got)
	}
}

// TestMessagesWheelKeepsPlace: the wheel moves the chat a few lines at a
// time. A line or two leaves the message a jump centered near the middle, so
// the list opens on it again; scrolled well past it, the list opens on the
// message the middle now shows.
func TestMessagesWheelKeepsPlace(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 148, 44)
	host.AppendMessage("user", "prompt")
	for i := range 20 {
		appendReply(host, fmt.Sprintf("reply %d at\none\ntwo\nthree\nfour", i), hostapi.TurnStat{})
	}
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	ov := openTurnRows(t, app, host)
	ov.selected = slices.IndexFunc(ov.visible, func(idx int) bool {
		return strings.Contains(ov.items[idx].label, "reply 10 at")
	})
	if ov.selected < 0 {
		t.Fatal("no reply 10 in the list")
	}
	app.Frame()
	want := ov.selectedAnchor()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
	app.Frame()
	if chat.follow {
		t.Fatal("the chat should sit off its tail")
	}
	wheel := func(n int) {
		for range n {
			app.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 40, Y: 10})
		}
		app.Frame()
	}
	for range 2 {
		wheel(1)
		got := openTurnRows(t, app, host).selectedAnchor()
		press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
		app.Frame()
		if got != want {
			t.Fatalf("a wheel line: opened on %d, want %d", got, want)
		}
	}
	wheel(8)
	got := openTurnRows(t, app, host).selectedAnchor()
	if got == want {
		t.Fatalf("scrolled well past it, still on %d", want)
	}
	mid := chat.top + (chat.viewH-1)/2
	if from, to := unitRows(chat.lines, got); from > mid || to < mid {
		t.Fatalf("opened on rows %d..%d, the view's middle is %d", from, to, mid)
	}
}

// messagesApp boots an app whose host knows /messages, which the ui tests
// otherwise boot without: the tree's rows are commands.
func messagesApp(t *testing.T) (*App, *hostapi.Host) {
	t.Helper()
	app, host := bootAgentApp(t, 110, 34)
	if _, err := host.Commands.Add(&hostapi.Command{
		Name: "messages", NArgs: hostapi.NArgsAny,
		GoFn: func(h *hostapi.Host, args string) error { return h.MessagesCommand(args) },
	}); err != nil {
		t.Fatal(err)
	}
	// a row's o opens the view a screen would: one that does not follow the
	// chat, drawn over the list it came from
	runLua(t, app, `zemi.command.add("call", function(args)
		zemi.ui.overlay.open({ id = "call:" .. args.args, title = "Command", body = "status ok" })
	end, { nargs = "*" })`)
	return app, host
}

// TestMessagesTree: /messages opens the levels down to the reader's place;
// h walks back up remembering each level's row, esc on a view returns to the
// level it came from, l on a leaf does nothing, and enter closes the menu
// keeping the chat's place.
func TestMessagesTree(t *testing.T) {
	t.Parallel()
	app, host := messagesApp(t)
	host.AppendMessage("user", "first")
	appendReply(host, "first reply", hostapi.TurnStat{})
	host.AppendMessage("user", "second")
	appendReply(host, "second reply with text", hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	pump := func() {
		for app.Pump() {
		}
		app.Frame()
	}
	top := func() *overlayState { return &app.overlays[len(app.overlays)-1] }
	ids := func() []string {
		var out []string
		for _, st := range app.overlays {
			out = append(out, st.o.ID)
		}
		return out
	}
	home := tea.KeyPressMsg{Code: tea.KeyHome}
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	pump()
	// the reader sits at the tail: the level of that turn, under the turns
	if n, id := len(app.overlays), top().o.ID; n != 2 || id != "messages turn 3" {
		t.Fatalf("%d overlays, top %q", n, id)
	}
	press(t, app, home)
	pump()
	turnRow := top().selected
	t.Logf("home: selected %d of %d, labels %v", turnRow, len(top().visible), top().labels)
	// h is the way back to the turns, and l returns to the row left there
	press(t, app, tea.KeyPressMsg{Code: 'h', Text: "h"})
	pump()
	if n, id := len(app.overlays), top().o.ID; n != 1 || id != "messages" {
		t.Fatalf("%d overlays after h, %q", n, id)
	}
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	pump()
	if n, id := len(app.overlays), top().o.ID; n != 2 || id != "messages turn 3" || top().selected != turnRow {
		var keys []string
		for _, i := range top().visible {
			keys = append(keys, itemKey(top().o.Items[i]))
		}
		t.Fatalf("%d overlays after l, %q at %d, want %d; lastSel %v keys %v byRow %v", n, id, top().selected, turnRow, app.lastSel, keys, app.byRow)
	}
	// l reads the message over this level; esc comes back to it as it was
	row := top().selected
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	waitPrompt(t, app, func() bool { return strings.HasPrefix(top().o.ID, "messages view ") })
	pump()
	if id := top().o.ID; !strings.HasPrefix(id, "messages view ") {
		t.Fatalf("l opened %q: %v", id, ids())
	}
	t.Logf("after l: %v", ids())
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	pump()
	if n, id := len(app.overlays), top().o.ID; n != 2 || id != "messages turn 3" || top().selected != row {
		t.Fatalf("esc on the view: %d overlays, %q at %d: %v", n, id, top().selected, ids())
	}
	// enter is the place itself, whichever level it is on: the menu closes
	chat := app.scrolls["chat"]
	chatTop, want := chat.top, top().selectedAnchor()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
	pump()
	if n := len(app.overlays); n != 0 {
		t.Fatalf("enter left %v", ids())
	}
	if chat.top != chatTop {
		t.Fatalf("enter moved the chat: top %d, want %d", chat.top, chatTop)
	}
	if got := bandedChatRows(app); len(got) != 0 {
		t.Fatalf("banded rows after enter: %q", got)
	}
	// reopening the menu follows the chat again, from the place it kept
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	pump()
	if got := top().selectedAnchor(); got != want {
		t.Fatalf("reopened on %d, want %d", got, want)
	}
	// h is one level back, and esc closes the whole menu at once, the chat
	// back where the menu found it
	if n, id := len(app.overlays), top().o.ID; n != 2 {
		t.Fatalf("%d overlays, %q", n, id)
	}
	press(t, app, tea.KeyPressMsg{Code: 'h', Text: "h"})
	pump()
	if n, id := len(app.overlays), top().o.ID; n != 1 || id != "messages" {
		t.Fatalf("h from a level: %d overlays, %q", n, id)
	}
	found := chat.top
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	pump()
	press(t, app, tea.KeyPressMsg{Code: 'j', Text: "j"})
	pump()
	if n := len(app.overlays); n != 2 {
		t.Fatalf("%d overlays, want the level reopened", n)
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	pump()
	if n := len(app.overlays); n != 0 {
		t.Fatalf("esc on a level left %d overlays, a menu closes whole", n)
	}
	if chat.top != found {
		t.Fatalf("esc left the chat at %d, it was found at %d", chat.top, found)
	}
}

// TestMessagesFollowsTheTranscript: a row that lands while the picker is open
// shows there, on the level that holds it, and the selection stays.
func TestMessagesFollowsTheTranscript(t *testing.T) {
	t.Parallel()
	app, host := messagesApp(t)
	host.AppendMessage("user", "watch this")
	appendReply(host, "first reply", hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	for app.Pump() {
	}
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		for app.Pump() {
		}
		app.Frame()
	}
	top := func() *overlayState { return &app.overlays[len(app.overlays)-1] }
	before := len(top().visible)
	sel := top().selected
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "notice", Text: "a notice landed"})
	for range 4 {
		for app.Pump() {
		}
		app.Frame()
	}
	if got := len(top().visible); got != before+1 {
		t.Fatalf("the level holds %d rows, want %d: %v", got, before+1, top().labels)
	}
	if !slices.Contains(top().labels, "a notice landed") {
		t.Fatalf("the new row is not in the level: %v", top().labels)
	}
	if top().selected != sel {
		t.Fatalf("the selection moved to %d, want %d", top().selected, sel)
	}
}

// TestMessagesRail: a turn's activity is one row in the turn view, the rail
// the chat folds; l opens its calls, the chat draws that run whole and bands
// the call the list names, and going back folds it again.
func TestMessagesRail(t *testing.T) {
	t.Parallel()
	app, host := messagesApp(t)
	host.AppendMessage("user", "walk the rail")
	for _, tool := range []struct{ name, text, label string }{
		{"read", "a.go", "read a"},
		{"grep", "Parse", "find Parse"},
		{"shell", "go test ./...", "run the tests"},
	} {
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: tool.name, Text: tool.text, Label: tool.label, Status: "ok"})
	}
	appendReply(host, "all green", hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	pump := func() {
		for range 3 {
			for app.Pump() {
			}
			app.Frame() // a compose settles the follow state the widget reads
			app.renderSync(t)
		}
		app.Frame()
	}
	top := func() *overlayState { return &app.overlays[len(app.overlays)-1] }
	frame := func() string { return ansi.Strip(app.Frame()) }
	// folded, the run is its digest alone
	if !strings.Contains(frame(), "1 file, 1 pattern, 1 command") || strings.Contains(frame(), "read a") {
		t.Fatalf("the settled run is not folded:\n%s", frame())
	}
	if err := host.MessagesCommand("turn 1"); err != nil {
		t.Fatal(err)
	}
	pump()
	// the turn's activity is one row: the run folds into it
	press(t, app, tea.KeyPressMsg{Code: 'k', Text: "k"})
	pump()
	if got := top().items[top().visible[top().selected]].row.Text; got != "3 steps" {
		t.Fatalf("the turn's selected row is %q, want the run's", got)
	}
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	pump()
	if id, n := top().o.ID, len(top().visible); id != "messages turn 1 rail 2" || n != 3 {
		t.Fatalf("rail view: %q, %d rows", id, n)
	}
	// the run draws whole, and only the picked call's row is banded
	for _, want := range []string{"read a", "find Parse", "run the tests"} {
		if !strings.Contains(frame(), want) {
			t.Fatalf("the rail did not unfold %q:\n%s", want, frame())
		}
	}
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "read a") {
		t.Fatalf("banded %q, want the picked call alone", got)
	}
	press(t, app, tea.KeyPressMsg{Code: 'j', Text: "j"})
	pump()
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "find Parse") {
		t.Fatalf("banded %q after a step, want the next call", got)
	}
	// l opens the call's own screen over the list: the chat keeps the run
	// open and bands the row under it, and esc comes back to the same preview
	row := top().selected
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	waitPrompt(t, app, func() bool { return top().o.ID == "call:3" })
	pump()
	if id := top().o.ID; id != "call:3" || !strings.Contains(frame(), "read a") {
		t.Fatalf("l opened %q with the run folded:\n%s", id, frame())
	}
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "find Parse") {
		t.Fatalf("banded %q under the call's view, want the picked call", got)
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	pump()
	if n, id := len(app.overlays), top().o.ID; n != 2 || id != "messages turn 1 rail 2" || top().selected != row {
		t.Fatalf("esc on the view: %d overlays, %q at %d, want the rail at %d", n, id, top().selected, row)
	}
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "find Parse") {
		t.Fatalf("banded %q after esc, want the picked call", got)
	}
	// enter is the row's place in the chat, never a level down: the menu
	// closes whole on the call it named, the chat keeping that place
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
	pump()
	if n := len(app.overlays); n != 0 {
		t.Fatalf("enter on a call left %d overlays", n)
	}
	if got := app.scrolls["chat"].focus; got != 3 {
		t.Fatalf("the chat kept %d as its place, want the call's row 3", got)
	}
	if err := host.MessagesCommand("turn 1"); err != nil {
		t.Fatal(err)
	}
	pump()
	press(t, app, tea.KeyPressMsg{Code: 'k', Text: "k"})
	pump()
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	pump()
	// back to the turn folds the run again, the band on its one row. The
	// list waits for the chat to render it folded, so no frame shows the run
	// open with the list gone: the fold lands first, the close after it.
	press(t, app, tea.KeyPressMsg{Code: 'h', Text: "h"})
	if app.rail != 0 || app.railClose == "" || len(app.overlays) != 2 {
		t.Fatalf("a closing rail: rail %d, closing %q, %d overlays", app.rail, app.railClose, len(app.overlays))
	}
	pump()
	if strings.Contains(frame(), "read a") {
		t.Fatalf("the run stayed open:\n%s", frame())
	}
	if got := bandedChatRows(app); len(got) != 1 || !strings.HasPrefix(got[0], "· ") {
		t.Fatalf("banded %q, want the run's row", got)
	}
	// a rail is a level of its turn, so esc on it closes the menu whole
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	pump()
	if n := len(app.overlays); n != 2 {
		t.Fatalf("%d overlays before esc", n)
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	pump()
	if n := len(app.overlays); n != 0 {
		t.Fatalf("esc on the rail left %d overlays", n)
	}
	if app.rail != 0 || strings.Contains(frame(), "read a") {
		t.Fatalf("rail %d, the run still open:\n%s", app.rail, frame())
	}
}
