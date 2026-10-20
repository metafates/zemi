package ui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

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

// bandedChatRows is the text of the chat rows the picker's message is banded
// on, as the last frame drew them.
func bandedChatRows(app *App) []string {
	chat := app.scrolls["chat"]
	var out []string
	for y := chat.rect.Min.Y; y < min(chat.rect.Max.Y, len(app.lastRows)); y++ {
		if row := app.lastRows[y]; len(row) > 0 && row[0].Fill == hostapi.FillBand {
			out = append(out, strings.TrimSpace(hostapi.SegsText(row)))
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
		if len(app.overlays) != 1 {
			t.Fatalf("%d overlays", len(app.overlays))
		}
		from, to := unitRows(chat.lines, app.overlays[0].selectedAnchor())
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
	app.Frame()
	chat := app.scrolls["chat"]
	ov := &app.overlays[0]
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
	// the same computation reads the tall message back on the next open
	want := ov.selectedAnchor()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
	app.Frame()
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	if got := app.overlays[0].selectedAnchor(); got != want {
		t.Fatalf("reopened on %d, want %d", got, want)
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

// TestMessagesReopenAll: every message, entered on and reopened, comes back
// on the same message. The viewport alone cannot say which one - centering
// clamps at the top and the tail, where many messages share one top row - so
// the chat keeps the row it was placed at until the reader moves it by hand.
func TestMessagesReopenAll(t *testing.T) {
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
		if len(app.overlays) != 1 {
			t.Fatalf("%d overlays", len(app.overlays))
		}
		return &app.overlays[0]
	}
	for k := range 30 {
		ov := open()
		ov.selected = 0 // away first, so picking k always moves the view
		app.Frame()
		ov.selected = k
		app.Frame()
		want := ov.selectedAnchor()
		press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
		app.Frame()
		if got := open().selectedAnchor(); got != want {
			t.Errorf("message %d: reopened on %d, want %d (top %d of %d, follow %v)", k, got, want, chat.top, chat.rows, chat.follow)
		}
		press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
		app.Frame()
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
	return app, host
}

// TestMessagesTree: l walks the conversation one level at a time - the turns,
// a turn's messages and rail, a row's actions - and h or esc walks back, the
// chat following whatever level names the row.
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
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	pump()
	if got := app.overlays[len(app.overlays)-1].o.ID; got != "messages" {
		t.Fatalf("the turns list is %q", got)
	}
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	pump()
	if n, id := len(app.overlays), app.overlays[len(app.overlays)-1].o.ID; n != 2 || !strings.HasPrefix(id, "messages turn ") {
		t.Fatalf("a turn is %d overlays deep, %q", n, id)
	}
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	pump()
	if n, id := len(app.overlays), app.overlays[len(app.overlays)-1].o.ID; n != 3 || !strings.HasSuffix(id, "#detail") {
		t.Fatalf("a row's actions are %d overlays deep, %q", n, id)
	}
	// an action opens its own view over the list it came from
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
	pump()
	top := app.overlays[len(app.overlays)-1].o.ID
	if !strings.HasPrefix(top, "messages view ") {
		t.Fatalf("the action opened %q", top)
	}
	// back to the list the action came from, then out of the browse
	press(t, app, tea.KeyPressMsg{Code: 'h', Text: "h"})
	pump()
	if n, id := len(app.overlays), app.overlays[len(app.overlays)-1].o.ID; n != 1 || id != "messages" {
		t.Fatalf("%d overlays after h, %q", n, id)
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	pump()
	if n := len(app.overlays); n != 0 {
		t.Fatalf("%d overlays after esc", n)
	}
}
