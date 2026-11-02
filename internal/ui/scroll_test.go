package ui

import (
	"fmt"
	"image/color"
	"log/slog"
	"math"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
)

// fillChat appends 30 user/reply pairs to the transcript.
func fillChat(host *hostapi.Host) {
	for i := 1; i <= 30; i++ {
		host.AppendMessage("user", fmt.Sprintf("message %d", i))
		appendReply(host, fmt.Sprintf("reply %d", i), hostapi.TurnStat{In: 1000, Out: 100, ElapsedMS: 1500, Model: "anthropic/claude-sonnet-4-5"})
	}
}

// press runs one keypress through the router and pumps the hook queue
// (host actions like zemi.ui.scroll.* dispatch their UI mutation). The
// queue is drained first: earlier host activity (message appends) may
// already have filled it.
func press(t *testing.T, app *App, key tea.KeyPressMsg) {
	t.Helper()
	for app.Pump() {
	}
	app.Update(key)
	for app.Pump() {
	}
}

// TestChatScrollKeys drives the default scroll bindings end to end:
// ctrl+u/pgdown move through the transcript (up half a view, down a page),
// ctrl+home/ctrl+b jump to the ends, and reaching the bottom re-arms tail
// follow.
func TestChatScrollKeys(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame()

	if !app.scrolls["chat"].follow {
		t.Fatalf("expected tail follow")
	}
	if !strings.Contains(app.Frame(), "reply 30") {
		t.Fatalf("tail view should show the latest reply")
	}

	press(t, app, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	if app.scrolls["chat"].follow {
		t.Fatalf("ctrl+u should leave tail follow")
	}
	if strings.Contains(app.Frame(), "reply 30") {
		t.Fatalf("scrolled view should not show the tail")
	}

	press(t, app, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if !app.scrolls["chat"].follow {
		t.Fatalf("pgdown into the bottom should re-arm follow")
	}
	if !strings.Contains(app.Frame(), "reply 30") {
		t.Fatalf("follow should show the tail again")
	}

	// the pair a list moves half a page with: up leaves the tail by half a
	// view and down comes back to it
	ctrlU, ctrlD := tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}, tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}
	press(t, app, ctrlU)
	st := app.scrolls["chat"]
	if half := max(st.viewH/2, 1); st.follow || st.top != st.rows-st.viewH-half {
		t.Fatalf("ctrl+u left the view at %d of %d rows, want half a view up", st.top, st.rows)
	}
	press(t, app, ctrlD)
	if !st.follow {
		t.Fatalf("ctrl+d should come back to the tail")
	}
	// the pair is the chat's whatever the draft holds: a drafted line keeps
	// its text and the view still moves
	app.composer.text, app.composer.cursor = "one\ntwo", len("one\ntwo")
	press(t, app, ctrlU)
	if app.composer.text != "one\ntwo" || st.follow {
		t.Fatalf("ctrl+u over a drafted line: %q, follow %v", app.composer.text, st.follow)
	}
	app.composer.text, app.composer.cursor = "", 0
	press(t, app, ctrlD)
	if !st.follow {
		t.Fatalf("ctrl+d should come back to the tail")
	}

	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	if app.scrolls["chat"].follow || app.scrolls["chat"].top != 0 {
		t.Fatalf("ctrl+home should pin the first row")
	}
	if !strings.Contains(app.Frame(), "message 1") {
		t.Fatalf("top view should show the first message")
	}

	press(t, app, tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	if !app.scrolls["chat"].follow {
		t.Fatalf("ctrl+b should re-arm follow")
	}
}

// TestChatScrollWheel pins the mouse wheel path: wheel up scrolls off the
// tail, wheel down re-arms follow at the bottom.
func TestChatScrollWheel(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame()

	app.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if app.scrolls["chat"].follow {
		t.Fatalf("wheel up should scroll off the tail")
	}
	for range 200 {
		app.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	}
	if !app.scrolls["chat"].follow {
		t.Fatalf("wheel down should re-arm follow at the bottom")
	}
}

// TestChatScrollStaysAnchored pins the reason scrollback exists: while the
// user is scrolled up, new transcript lines must not move the visible
// window (the tail keeps filling underneath).
func TestChatScrollStaysAnchored(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame()

	press(t, app, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	top := app.scrolls["chat"].top
	before := app.Frame()

	for i := 31; i <= 35; i++ {
		host.AppendMessage("user", fmt.Sprintf("message %d", i))
	}
	app.renderSync(t)
	after := app.Frame()

	if app.scrolls["chat"].follow {
		t.Fatalf("new lines must not yank a scrolled view")
	}
	// the chat window must not move; the statusline may, its scroll
	// indicator counts what is now below
	if app.scrolls["chat"].top != top || withoutStatus(after) != withoutStatus(before) {
		t.Fatalf("scrolled frame should stay anchored as content grows")
	}
	if !strings.Contains(after, "ctrl+b") {
		t.Fatalf("a scrolled view should say how to get back:\n%s", after)
	}
}

// TestChatScrollShrinks: a scrolled view whose rows shrink by more than its
// offset (a fill restarting, a collapsed block) guesses a row above the
// first line and must land on the content, not panic.
func TestChatScrollShrinks(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	st := app.scrolls["chat"]
	st.top, st.rows = 2, st.rows+400 // the last frame drew 400 rows more
	app.frameGen++
	app.renderSync(t)
	if f := app.Frame(); !strings.Contains(f, "message") {
		t.Fatalf("the shrunk transcript is not drawn:\n%s", f)
	}
}

func withoutStatus(frame string) string {
	rows := strings.Split(frame, "\r\n")
	return strings.Join(rows[:len(rows)-1], "\r\n")
}

// TestAgentViewKeepsTheSessionPlace: a run's transcript scrolls apart from
// the session - it opens at its own tail even with an edge parked on the
// session, esc lands the session on the place it was left at, not the
// bottom, and a switch to another run parks the session's place, not the
// first transcript's.
func TestAgentViewKeepsTheSessionPlace(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 100, 30)
	fillChat(host)
	app.renderSync(t)
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	app.Frame()
	session := app.scrolls[app.scrollDef]
	if session.follow || session.top == 0 {
		t.Fatalf("setup: the session did not scroll back (follow %v, top %d)", session.follow, session.top)
	}
	top := session.top

	db := testStore(t, host)
	stored := func(name string) string {
		id := store.ShortID()
		if err := db.CreateSession(id, store.ShortID(), name, "explore"); err != nil {
			t.Fatal(err)
		}
		for i := range 40 {
			addRunMessage(t, db, id, fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
				fantasy.TextPart{Text: fmt.Sprintf("%s line %d", name, i)},
			}})
		}
		return id
	}
	view := func(id string) {
		t.Helper()
		if err := host.ViewAgent(id); err != nil {
			t.Fatal(err)
		}
		app.renderSync(t)
		app.Frame()
	}
	first := stored("run")
	// a ScrollTop parked while the transcript showed must neither drag esc's
	// session to its top nor the next transcript: ctrl+home inside the view,
	// esc, and back in
	view(first)
	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	app.Frame()
	if st := app.scrolls[app.scrollDef]; st.follow || st.top != 0 {
		t.Fatalf("setup: ctrl+home did not reach the transcript's top (follow %v, top %d)", st.follow, st.top)
	}
	if back := host.LeaveAgentView(); back != "" {
		t.Fatalf("the leave ran back %q", back)
	}
	app.renderSync(t)
	app.Frame()
	if st := app.scrolls[app.scrollDef]; st != session || st.follow || st.top != top {
		t.Fatalf("esc landed on follow %v top %d, want the left place %d", st.follow, st.top, top)
	}
	view(first)
	if st := app.scrolls[app.scrollDef]; !st.follow || st.top == 0 {
		t.Fatalf("the re-entered transcript opened at top %d follow %v, want its tail", st.top, st.follow)
	}
	// one view to another: the session's place stays parked, the first
	// transcript's does not stand in for it
	view(stored("other"))
	if st := app.scrolls[app.scrollDef]; st == session || !st.follow {
		t.Fatalf("the second transcript draws with a foreign state (follow %v)", st.follow)
	}
	if back := host.LeaveAgentView(); back != "" {
		t.Fatalf("the leave ran back %q", back)
	}
	app.renderSync(t)
	app.Frame()
	if st := app.scrolls[app.scrollDef]; st != session || st.follow || st.top != top {
		t.Fatalf("esc from the second run landed on follow %v top %d, want %d", st.follow, st.top, top)
	}
}

// TestAgentViewSendSnapsToTail: sending from a run that is over shows the
// session at its tail, where the message and its reply land, not at the
// place the session was left at.
func TestAgentViewSendSnapsToTail(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 100, 30)
	fillChat(host)
	app.renderSync(t)
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	app.Frame()

	db := testStore(t, host)
	id := store.ShortID()
	if err := db.CreateSession(id, store.ShortID(), "run", "explore"); err != nil {
		t.Fatal(err)
	}
	addRunMessage(t, db, id, fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
		fantasy.TextPart{Text: "it reads fine"},
	}})
	if err := host.ViewAgent(id); err != nil {
		t.Fatal(err)
	}
	app.renderSync(t)
	app.Frame()
	app.sendMessage("one more")
	app.renderSync(t)
	app.Frame()
	if st := app.scrolls[app.scrollDef]; !st.follow {
		t.Fatalf("the send left the session off the tail (top %d)", st.top)
	}
}

// TestChatScrollFollows pins the snap-back: sending a message, or a new
// transcript (a reset's wipe), returns the view to the tail.
func TestChatScrollFollows(t *testing.T) {
	for name, act := range map[string]func(*App, *hostapi.Host) error{
		"submit": func(app *App, _ *hostapi.Host) error {
			app.composer.text = "next question"
			app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			return nil
		},
		"reset": func(_ *App, host *hostapi.Host) error {
			// a reset replaces the window, which re-opens at the tail
			host.SetTranscript(nil)
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app, host := bootAgentApp(t, 80, 14)
			fillChat(host)
			app.renderSync(t)
			app.Frame()
			press(t, app, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
			if app.scrolls["chat"].follow {
				t.Fatalf("precondition: scrolled off the tail")
			}
			if err := act(app, host); err != nil {
				t.Fatal(err)
			}
			for app.Pump() {
			}
			if !app.scrolls["chat"].follow {
				t.Fatalf("should snap back to the tail")
			}
		})
	}
}

// TestChatScrollShortContent: content shorter than the viewport cannot
// scroll; ctrl+u is a no-op, and the transcript starts one row below the
// region's own first row, which is the pad above it.
func TestChatScrollShortContent(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	host.AppendMessage("user", "only one")
	app.renderSync(t)
	app.Frame()

	for _, key := range []tea.KeyPressMsg{{Code: 'u', Mod: tea.ModCtrl}, {Code: tea.KeyHome, Mod: tea.ModCtrl}} {
		press(t, app, key)
		if !app.scrolls["chat"].follow {
			t.Fatalf("short content cannot scroll: %s", key)
		}
	}
	if !strings.Contains(app.Frame(), "only one") {
		t.Fatalf("message must stay visible")
	}
	st := app.scrolls["chat"]
	pad := strings.TrimSpace(hostapi.SegsText(app.lastRows[st.rect.Min.Y]))
	row := strings.TrimSpace(hostapi.SegsText(app.lastRows[st.rect.Min.Y+1]))
	if pad != "" || !strings.Contains(row, "only one") {
		t.Fatalf("a short transcript starts one row below the region's first row: %q then %q", pad, row)
	}
}

// TestChatScrollAnchorsThroughRelayout: a narrower chat rewraps the lines
// above the viewport and a click unfolds a digest among them; both
// change how many rows sit above it, and it must keep showing the content
// it showed, not the row number it showed.
func TestChatScrollAnchorsThroughRelayout(t *testing.T) {
	t.Parallel()
	visible := func(frame string) string {
		t.Helper()
		for row := range strings.SplitSeq(frame, "\r\n") {
			if i := strings.Index(ansi.Strip(row), "message "); i >= 0 {
				// the scrollbar ends the row, and the padding before it
				// depends on the width
				return strings.TrimRight(ansi.Strip(row)[i:], " │█")
			}
		}
		t.Fatalf("no message row on screen:\n%s", frame)
		return ""
	}
	app, host := bootAgentApp(t, 100, 14)
	for i := 1; i <= 20; i++ {
		host.AppendMessage("user", fmt.Sprintf("message %d", i))
		appendReasoning(host, fmt.Sprintf("thought %d line one\nthought %d line two\n", i, i), hostapi.TurnStat{ThoughtMS: 4200})
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "a.go", Status: "ok"})
		appendReply(host, fmt.Sprintf("reply %d", i), hostapi.TurnStat{In: 1000, Out: 100, ElapsedMS: 1500})
	}
	app.renderSync(t)
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	press(t, app, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	want := visible(app.Frame())

	app.Update(tea.WindowSizeMsg{Width: 40, Height: 14})
	app.renderSync(t)
	if got := visible(app.Frame()); got != want {
		t.Fatalf("resize moved the view from %q to %q", want, got)
	}

	clickChat(t, app, "turn:2")
	app.renderSync(t)
	if got := visible(app.Frame()); got != want {
		t.Fatalf("unfolding a digest moved the view from %q to %q", want, got)
	}
}

// TestChatFoldLeavesTheTail: a click that redraws the chat - a digest
// opening over its rail - leaves the tail, so the rows it adds grow under
// the reader's place instead of scrolling it away, past the view's own
// height.
func TestChatFoldLeavesTheTail(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 30)
	host.AppendMessage("user", "update @.golangci.yaml so make lint works again")
	for i := range 6 {
		appendReasoning(host, fmt.Sprintf("look at the config %d\n", i), hostapi.TurnStat{ThoughtMS: 2800})
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: fmt.Sprintf("a%d.go", i), Status: "ok"})
	}
	appendReply(host, "Root cause found: the config names linters the way v2.11 did.\nBut v2.13 renamed several.", hostapi.TurnStat{Out: 18800, ElapsedMS: 4000})
	appendReasoning(host, "one more check\n", hostapi.TurnStat{ThoughtMS: 800})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "bash", Text: "make lint", Status: "ok"})
	appendReply(host, "make lint passes with no issues and no deprecation warnings.\nThe disable list named linters by their pre-v2.13 names, so default: all kept on\nthe renamed ones as separate linters, and gofumpt.extra-rules was replaced.", hostapi.TurnStat{Out: 18800, ElapsedMS: 4000})
	app.renderSync(t)
	app.Frame()

	st := app.scrolls["chat"]
	if !st.follow || st.top != 0 || st.rows >= st.viewH {
		t.Fatalf("precondition: top %d, follow %v, %d rows over %d", st.top, st.follow, st.rows, st.viewH)
	}
	// the region's first row, without the scrollbar's own cell
	head := func() string {
		r := app.lastRows[st.rect.Min.Y]
		return strings.TrimSpace(hostapi.SegsText(r[:len(r)-1]))
	}
	first := head()

	digest := slices.IndexFunc(strings.Split(ansi.Strip(app.Frame()), "\n"), func(r string) bool {
		return strings.Contains(r, "· 6 files")
	})
	if digest < 0 {
		t.Fatal("no digest row on screen")
	}
	clickAt(app, 5, digest)
	app.renderSync(t)
	app.Frame()

	if st.follow || st.top != 0 {
		t.Fatalf("the fold scrolled the view: top %d, follow %v", st.top, st.follow)
	}
	if st.rows <= st.viewH {
		t.Fatalf("the rail did not grow past the view: %d rows over %d", st.rows, st.viewH)
	}
	got := head()
	if got != first {
		t.Fatalf("the first row moved from %q to %q", first, got)
	}
}

// TestChatScrollAnchorsThroughFill: a long chat fills its latest rows first,
// and a reader who scrolled up in them keeps their place when the older rows
// land above, though every reply repeats the same lines.
func TestChatScrollAnchorsThroughFill(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	for i := 1; i <= 30; i++ {
		host.AppendMessage("user", fmt.Sprintf("message %d", i))
		appendReply(host, "line a\n\nline b\n\nline c\n\nline d\n\nline e", hostapi.TurnStat{Out: 100, ElapsedMS: 1500})
	}
	app.renderSync(t)
	app.Frame()
	var chat shownWidget
	for _, w := range app.shownWidgets() {
		if w.Region == app.scrolls["chat"].region {
			chat = w
		}
	}
	full := app.caches[chat.ID].lines
	fill := func(lines [][]hostapi.Segment, more bool) {
		app.onWidgetRendered(widgetRender{id: chat.ID, lines: lines, more: more, width: chat.width, height: chat.height, gen: app.frameGen})
		app.Frame()
	}
	// the text on screen: the bar's thumb shrinks as the rows land
	text := func() string { return strings.ReplaceAll(ansi.Strip(app.Frame()), "█", "│") }
	// the first pass draws the last three turns
	first := slices.IndexFunc(full, func(l []hostapi.Segment) bool { return strings.Contains(hostapi.SegsText(l), "message 28") })
	fill(full[first:], true)
	press(t, app, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	want, below := text(), app.rowsBelow()
	fill(full, false)
	if got := text(); got != want || app.rowsBelow() != below {
		t.Fatalf("the fill moved the view (%d rows below, want %d):\n%s\nwant:\n%s", app.rowsBelow(), below, got, want)
	}
}

// TestChatStickyPrompt: while a turn's rows fill the view, its prompt's
// first row stands in for the chat's top row, cut with an ellipsis when the
// prompt has more rows and copied once; it lights like a header, copies
// nothing, and a click or alt+up scrolls back to the prompt. A report's
// notice heads its turn the same way.
func TestChatStickyPrompt(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	const prompt = "fix the flaky TestResume in internal/store: it fails about one run in twenty on the build machine"
	host.AppendMessage("user", prompt)
	for i := range 20 {
		appendReply(host, fmt.Sprintf("step %d", i), hostapi.TurnStat{})
	}
	app.renderSync(t)
	app.Frame()
	st := app.scrolls["chat"]
	y := st.rect.Min.Y
	row := func() string {
		return strings.TrimRight(hostapi.SegsText(app.lastRows[y][:len(app.lastRows[y])-1]), " ")
	}
	if !st.follow || !strings.HasPrefix(row(), "▌ fix the flaky") || !strings.HasSuffix(row(), "…") {
		t.Fatalf("following, the top row is %q", row())
	}
	// the process-wide malloc count AllocsPerRun reads moves with the app's
	// other goroutines, so the copy's identity says it was not made again
	first := &st.pins[0][0]
	if st.pinRows(st.lines, st.rect.Dx()); &st.pins[0][0] != first {
		t.Fatal("an unchanged pin copies its row again")
	}
	// a toast over the pin takes its own cells and cuts the pin's short
	app.pushToast(slog.LevelInfo, "saved", "")
	app.Frame()
	if _, _, onToast := app.target(70, y); onToast {
		t.Fatal("a toast over the pin acts as the pin")
	}
	if _, _, beside := app.target(20, y); !beside {
		t.Fatal("the pin beside a toast does not act")
	}
	app.toasts = nil

	if dragTo(app, 3, y, 20, y); app.selectionText() != "" {
		t.Fatalf("the pin copied %q", app.selectionText())
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})

	app.Update(tea.MouseMotionMsg{X: 5, Y: y})
	app.Frame()
	if over := app.lastRows[y][1].Over; over != "Hover" {
		t.Fatalf("hovered pin drew %q over its text", over)
	}

	// alt+up and a click on the pin both land on the prompt; alt+down with no
	// prompt below returns to the tail
	for _, jump := range []func(){
		func() { press(t, app, tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt}) },
		func() { clickAt(app, 5, y) },
	} {
		jump()
		frame := ansi.Strip(app.Frame())
		if st.follow || !headerAt(st.lines, st.top) || len(st.pins) != 0 || strings.Count(frame, "fix the flaky") != 1 {
			t.Fatalf("the jump left top %d (follow %v):\n%s", st.top, st.follow, frame)
		}
		press(t, app, tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModAlt})
		if app.Frame(); !st.follow {
			t.Fatalf("alt+down left top %d short of the tail", st.top)
		}
	}

	// a report's notice pins nothing: only a prompt does
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: hostapi.RowNotice, Text: "Background agent a1 (explore) finished"})
	for i := range 20 {
		appendReply(host, fmt.Sprintf("answer %d", i), hostapi.TurnStat{})
	}
	app.renderSync(t)
	if app.Frame(); strings.Contains(row(), "Background agent a1") {
		t.Fatalf("after a report, the top row is %q", row())
	}
}

// TestChatStickyDigest: inside an opened run the prompt and the run's digest
// pin over the chat's top rows, in that order, on the panel; from the run's
// last row on, or with the run folded, the prompt pins alone. A click on the
// digest's pin puts the digest itself under the prompt's.
// TestChatFade: the rows under the pins sink toward the canvas while rows lie
// scrolled above them, and the last rows while rows lie below, deepest at the
// edge and easing off as the last rows come into view; the pins, the bar and
// an edge with nothing past it do not.
func TestChatFade(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 24) // a chat over 12 rows fades its full 4
	host.AppendMessage("user", "look around")
	for i := range 30 {
		appendReply(host, fmt.Sprintf("answer %d", i), hostapi.TurnStat{})
	}
	app.renderSync(t)
	app.Frame()
	st := app.scrolls["chat"]
	// the top six rows and the bottom six, top down
	fades := func() (top, bottom []float64) {
		app.Frame()
		for _, row := range app.lastRows[st.rect.Min.Y : st.rect.Min.Y+6] {
			top = append(top, row[0].Fade)
		}
		for _, row := range app.lastRows[st.rect.Max.Y-6 : st.rect.Max.Y] {
			bottom = append(bottom, row[0].Fade)
		}
		return top, bottom
	}
	none := func(f []float64) bool { return !slices.ContainsFunc(f, func(x float64) bool { return x != 0 }) }
	if top, bottom := fades(); !slices.Equal(top, []float64{0, 0.8, 0.6, 0.4, 0.2, 0}) || !none(bottom) {
		t.Fatalf("at the tail only the top fades, pin aside: %v %v", top, bottom)
	}
	app.scrollRows("chat", -30)
	if top, bottom := fades(); !slices.Equal(top, []float64{0, 0.8, 0.6, 0.4, 0.2, 0}) || !slices.Equal(bottom, []float64{0, 0, 0.2, 0.4, 0.6, 0.8}) {
		t.Fatalf("both edges fade, pin aside: %v %v", top, bottom)
	}
	app.scrollRows("chat", -st.top)
	if top, bottom := fades(); !none(top) || slices.Equal(bottom, make([]float64, 6)) {
		t.Fatalf("at the top only the bottom fades: %v %v", top, bottom)
	}
	// one row past the bottom edge: the ramp at a quarter of its depth
	app.scrollRows("chat", st.rows-st.top-st.viewH-1)
	if _, bottom := fades(); math.Abs(bottom[5]-0.2) > 1e-9 || math.Abs(bottom[2]-0.05) > 1e-9 {
		t.Fatalf("the fade does not ease off near the edge: %v", bottom)
	}
	// a modal sinks the backdrop toward the canvas and keeps its own styles
	was := app.lastRows[st.rect.Max.Y-1][0].HL
	app.openOverlay(hostapi.Overlay{ID: "t", Title: "T", Body: "x"})
	app.Frame()
	if row := app.lastRows[st.rect.Max.Y-1]; row[0].HL != was || math.Abs(row[0].Fade-modalFade) > 1e-9 {
		t.Fatalf("the backdrop under a modal: %+v, want %s at fade %v", row[0], was, modalFade)
	}
	app.closeOverlayByID("t")
	// the bar cell keeps its color
	app.Frame()
	for _, row := range app.lastRows[st.rect.Min.Y : st.rect.Min.Y+3] {
		if last := row[len(row)-1]; last.Bar && last.Fade != 0 {
			t.Fatalf("the bar fades: %+v", last)
		}
	}
}

// TestBoxFade: a box's scrolled view sinks the rows at an edge content lies
// past toward the panel, the surface it sits on and not the canvas, deepest
// at the edge; a list keeps its selected row clear of the ramp.
func TestBoxFade(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 24)
	items := make([]hostapi.OverlayItem, 30)
	for i := range items {
		items[i] = hostapi.OverlayItem{Text: fmt.Sprintf("row %02d", i)}
	}
	app.openOverlay(hostapi.Overlay{ID: "l", Title: "List", Origin: lua.OriginBuiltin, Items: items})
	st := &app.overlays[0]
	at := func(i int) float64 {
		if row := st.rows[i]; len(row) > 0 {
			return row[0].Fade
		}
		t.Fatalf("row %d of the list is empty", i)
		return 0
	}
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
	// at the top the selection is the first row and only the rows below fade,
	// the last of them deepest
	app.Frame()
	n := len(st.rows)
	if n < 4 || st.selected != 0 || app.chromeRect.Empty() {
		t.Fatalf("the list opened showing %d rows, selected %d, box %v", n, st.selected, app.chromeRect)
	}
	if !near(at(n-1), 2.0/3) || !near(at(n-2), 1.0/3) || at(n-3) != 0 {
		t.Fatalf("the bottom ramp is %v %v %v, want 0, 1/3, 2/3", at(n-3), at(n-2), at(n-1))
	}
	// every row of the box is on the panel, so a fade of its own sinks there
	for y := app.chromeRect.Min.Y; y < app.chromeRect.Max.Y; y++ {
		for _, s := range app.lastRows[y] {
			if strings.TrimSpace(s.Text) != "" && !s.OnPanel {
				t.Fatalf("the box's row %d is not on the panel: %+v", y, s)
			}
		}
	}
	// scrolled to the middle both edges fade, and the selected row stays off
	// the ramp: the rows the list left below it take it
	for range 15 {
		app.overlayKey("down", "")
	}
	app.Frame()
	n = len(st.rows)
	if st.view.top == 0 || !near(at(0), 2.0/3) || !near(at(1), 1.0/3) {
		t.Fatalf("at top %d the top ramp is %v %v, want 2/3, 1/3", st.view.top, at(0), at(1))
	}
	if !near(at(n-2), 1.0/3) || !near(at(n-1), 2.0/3) {
		t.Fatalf("the bottom ramp is %v %v, want 1/3, 2/3", at(n-2), at(n-1))
	}
	sel := -1
	for i, row := range st.rows {
		if len(row) == 0 || row[0].Hit != st.selected+1 {
			continue
		}
		sel = i
		if row[0].Fade > 0 {
			t.Fatalf("the selected row %d of %d is on the ramp: %+v", i, n, row[0])
		}
	}
	if sel < 2 || sel > n-3 {
		t.Fatalf("the selected row %d of %d sits on the edge", sel, n)
	}
	// the ramp steps over the rows a window left empty: a list cut to whole
	// items pads past its content with nil, and the last row that shows
	// something is still the deepest
	rows := [][]hostapi.Segment{{{Text: "a"}}, {{Text: "b"}}, nil, nil, nil, nil}
	edgeRamp(rows, 0, 14, boxFade, func(i int) bool { return len(rows[i]) == 0 })
	if !near(rows[0][0].Fade, 1.0/3) || !near(rows[1][0].Fade, 2.0/3) {
		t.Fatalf("the padded ramp is %v %v, want 1/3, 2/3", rows[0][0].Fade, rows[1][0].Fade)
	}
	// an overlay's body text scrolls through the same viewport, so it fades
	// at the top once rows lie above it
	app.overlays = nil
	app.openOverlay(hostapi.Overlay{ID: "b", Title: "Body", Origin: lua.OriginBuiltin, Body: strings.Repeat("line\n", 40)})
	app.Frame()
	for range 5 {
		app.overlayKey("ctrl+d", "")
	}
	app.Frame()
	cell := hostapi.Segment{}
	for _, s := range app.lastRows[app.chromeRect.Min.Y+1] {
		if s.OnPanel && !s.Line && strings.TrimSpace(s.Text) != "" {
			cell = s
			break
		}
	}
	if !near(cell.Fade, 2.0/3) {
		t.Fatalf("the body's first row is not on the ramp: %+v", cell)
	}
}

// TestFadeGround: a faded segment sinks toward the surface it sits on, the
// canvas or a box's panel.
func TestFadeGround(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 24)
	theme := app.thm.active()
	if theme == nil {
		t.Fatal("no theme")
	}
	fg, bg, panel := theme.Palette.Fg, theme.Palette.Bg, theme.Palette.Panel
	for _, c := range []struct {
		name  string
		panel bool
		want  color.Color
	}{
		{"the canvas", false, hostapi.Mix(fg, bg, 0.5)},
		{"a box's panel", true, hostapi.Mix(fg, panel, 0.5)},
	} {
		if got := app.segStyle("Normal", "", hostapi.FillPanel, 0.5, c.panel).Fg; got != c.want {
			t.Fatalf("%s: fg = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestScrollbarSurface: the bar cell keeps the surface of the cell beside it,
// so a banded row's band runs to the box's edge instead of stopping a cell
// short of the bar drawn over it.
func TestScrollbarSurface(t *testing.T) {
	b := scrollbar{h: 10, top: 0, total: 40, width: 20}
	row := []hostapi.Segment{{Text: strings.Repeat(" ", 19), Fill: hostapi.FillBand}}
	got := b.row(row, 0)
	if last := got[len(got)-1]; !last.Bar || last.Fill != hostapi.FillBand {
		t.Fatalf("the bar cell %+v does not keep the row's surface", last)
	}
}

// TestThinkingFade: the live thinking tail sinks at its top while it hides
// lines above, over the chat's own four-row ramp and easing off as the hidden
// lines come back; a tail that fits keeps its colors, the rail bar never
// sinks, and a widget's deeper sink outlives the region's.
func TestThinkingFade(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 24)
	host.AppendMessage("user", "look around")
	tail := func(reasoning string) []float64 {
		host.SeedStream(hostapi.StreamSnapshot{Active: true, Reasoning: reasoning, Thinking: true})
		app.renderSync(t)
		app.Frame()
		st := app.scrolls["chat"]
		head := slices.IndexFunc(st.lines, func(r []hostapi.Segment) bool {
			return strings.HasSuffix(strings.TrimSpace(hostapi.SegsText(r)), "thinking")
		})
		if head < 0 {
			t.Fatal("no thinking row")
		}
		var out []float64
		for i := head + 1; i <= head+4 && i < len(st.lines); i++ {
			row := app.lastRows[st.rect.Min.Y+i-st.top]
			if strings.TrimSpace(hostapi.SegsText(row)) == "│" {
				break // the air row framing the block ends it
			}
			if row[0].Fade != 0 {
				t.Fatalf("the rail bar sinks: %+v", row[0])
			}
			out = append(out, row[1].Fade)
		}
		return out
	}
	for _, c := range []struct {
		name string
		text string
		want []float64
	}{
		{"a tail that fits keeps its colors", "one line of thought", []float64{0}},
		{"one hidden line eases the ramp", "a\nb\nc\nd\ne", []float64{0.2, 0.15, 0.1, 0.05}},
		{"four hidden lines sink it fully", "a\nb\nc\nd\ne\nf\ng\nh", []float64{0.8, 0.6, 0.4, 0.2}},
		{"more hidden lines sink no deeper", strings.Repeat("a line of thought\n", 12), []float64{0.8, 0.6, 0.4, 0.2}},
	} {
		got := tail(c.text)
		if len(got) != len(c.want) {
			t.Fatalf("%s: %d rows, want %d: %v", c.name, len(got), len(c.want), got)
		}
		for i := range c.want {
			if math.Abs(got[i]-c.want[i]) > 1e-9 {
				t.Fatalf("%s: %v, want %v", c.name, got, c.want)
			}
		}
	}
	// a row the widget sank deeper keeps its sink: the region's edge is shallower
	row := faded([]hostapi.Segment{{Text: "a", Fade: 0.9}, {Text: "b", Bar: true}}, 0.4)
	if row[0].Fade != 0.9 || row[1].Fade != 0 {
		t.Fatalf("the region brightened a sunk row: %+v", row)
	}
}

func TestChatStickyDigest(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	host.AppendMessage("user", "look around")
	appendReasoning(host, "where", hostapi.TurnStat{ThoughtMS: 8000})
	for i := range 12 {
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "shell", Text: "ls", Label: fmt.Sprintf("list dir %d", i), Status: "ok"})
	}
	for i := range 12 {
		appendReply(host, fmt.Sprintf("answer %d", i), hostapi.TurnStat{})
	}
	const prompt, digest = "▌ look around", "· 12 commands"
	var st *scrollState
	// pins scrolls the chat so the first row holding at tops the view, and
	// reads the pins drawn over it, each on the panel running on to the bar
	pins := func(at string) []string {
		app.renderSync(t)
		app.Frame()
		st = app.scrolls["chat"]
		i := slices.IndexFunc(st.lines, func(r []hostapi.Segment) bool { return strings.Contains(hostapi.SegsText(r), at) })
		if i < 0 {
			t.Fatal("no such row")
		}
		app.scrollRows("chat", i-st.top)
		app.Frame()
		var out []string
		for _, row := range app.lastRows[st.rect.Min.Y:] {
			if row[0].HL != "Pinned" {
				break
			}
			if row[0].Fill != hostapi.FillPanel || row[len(row)-2].Fill != hostapi.FillPanel {
				t.Fatalf("a pin off the panel: %+v", row)
			}
			out = append(out, hostapi.SegsText(row))
		}
		return out
	}
	app.renderSync(t)                                 // the first render forgets what was opened before it
	for _, id := range []string{"turn:2", "more:2"} { // the run open, and whole
		clickChat(t, app, id)
	}
	for _, c := range []struct {
		at   string
		want []string
	}{
		{"list dir 5", []string{prompt, digest}},
		{"list dir 9", []string{prompt, digest}},
		{"list dir 11", []string{prompt}},
		{"answer 2", []string{prompt}},
	} {
		if got := pins(c.at); !slices.EqualFunc(got, c.want, strings.HasPrefix) {
			t.Fatalf("at %q the pins read %q, want %q", c.at, got, c.want)
		}
	}

	pins("list dir 5")
	clickAt(app, 5, st.rect.Min.Y+1)
	app.Frame()
	if row := app.lastRows[st.rect.Min.Y+1]; row[0].HL == "Pinned" || !strings.HasPrefix(hostapi.SegsText(row), digest) || !headerAt(st.lines, st.top+1) {
		t.Fatalf("the click on the digest's pin left top %d with %q under the prompt", st.top, hostapi.SegsText(row))
	}

	clickChat(t, app, "turn:2")
	if got := pins("answer 2"); !slices.EqualFunc(got, []string{prompt}, strings.HasPrefix) {
		t.Fatalf("with the run folded the pins read %q", got)
	}
}

// TestScrollPendingEdge: an edge issued before the chat's rows arrive lands
// on them: "top" stays parked on the first row until the chat scrolls again,
// "bottom" follows the tail.
func TestScrollPendingEdge(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	app.applyScroll(hostapi.ScrollTop, "chat")
	rows := func(tag string, n int) (*scrollState, string) {
		t.Helper()
		for len(host.Session().Transcript) < n {
			host.AppendMessage("user", fmt.Sprintf("%s row %d", tag, len(host.Session().Transcript)+1))
		}
		app.renderSync(t)
		return app.scrolls["chat"], ansi.Strip(app.Frame())
	}
	if st, frame := rows("a", 40); st.top != 0 || st.follow || !strings.Contains(frame, "a row 1 ") {
		t.Fatalf("a top before the rows arrived: top %d, follow %v\n%s", st.top, st.follow, frame)
	}
	if st, frame := rows("b", 60); st.top != 0 || !strings.Contains(frame, "a row 1 ") {
		t.Fatalf("new rows under a parked top: top %d\n%s", st.top, frame)
	}
	app.scrollRows("chat", 3)
	app.Frame()
	if st, _ := rows("b", 61); st.top != 3 {
		t.Fatalf("a scroll leaves the park: top %d", st.top)
	}
	app.applyScroll(hostapi.ScrollBottom, "chat")
	if st, frame := rows("c", 70); !st.follow || !strings.Contains(frame, "c row 70") {
		t.Fatalf("a bottom follows the tail: follow %v\n%s", st.follow, frame)
	}
}
