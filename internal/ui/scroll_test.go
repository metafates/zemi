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

// fillChat appends 30 user/reply pairs to the transcript.
func fillChat(host *hostapi.Host) {
	for i := 1; i <= 30; i++ {
		host.AppendMessage("user", fmt.Sprintf("message %d", i))
		appendReply(host, fmt.Sprintf("reply %d", i), hostapi.TurnStat{In: 1000, Out: 100, ElapsedMS: 1500})
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
// pgup/pgdown page through the transcript, ctrl+home/ctrl+end jump to the
// ends, and reaching the bottom re-arms tail follow.
func TestChatScrollKeys(t *testing.T) {
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

	press(t, app, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if app.scrolls["chat"].follow {
		t.Fatalf("pgup should leave tail follow")
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

	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	if app.scrolls["chat"].follow || app.scrolls["chat"].top != 0 {
		t.Fatalf("ctrl+home should pin the first row")
	}
	if !strings.Contains(app.Frame(), "message 1") {
		t.Fatalf("top view should show the first message")
	}

	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
	if !app.scrolls["chat"].follow {
		t.Fatalf("ctrl+end should re-arm follow")
	}
}

// TestChatScrollWheel pins the mouse wheel path: wheel up scrolls off the
// tail, wheel down re-arms follow at the bottom.
func TestChatScrollWheel(t *testing.T) {
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
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame()

	press(t, app, tea.KeyPressMsg{Code: tea.KeyPgUp})
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
	if !strings.Contains(after, "ctrl+end") {
		t.Fatalf("a scrolled view should say how to get back:\n%s", after)
	}
}

// TestChatScrollShrinks: a scrolled view whose rows shrink by more than its
// offset (a fill restarting, a collapsed block) guesses a row above the
// first line and must land on the content, not panic.
func TestChatScrollShrinks(t *testing.T) {
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyPgUp})
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

// TestChatScrollFollows pins the snap-back: sending a message, or a new
// transcript (new, resume, rewind), returns the view to the tail.
func TestChatScrollFollows(t *testing.T) {
	for name, act := range map[string]func(*App, *hostapi.Host) error{
		"submit": func(app *App, _ *hostapi.Host) error {
			app.composer.text = "next question"
			app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			return nil
		},
		"new session": func(_ *App, host *hostapi.Host) error { return host.NewSession() },
	} {
		t.Run(name, func(t *testing.T) {
			app, host := bootAgentApp(t, 80, 14)
			fillChat(host)
			app.renderSync(t)
			app.Frame()
			press(t, app, tea.KeyPressMsg{Code: tea.KeyPgUp})
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
// scroll; pgup is a no-op.
func TestChatScrollShortContent(t *testing.T) {
	app, host := bootAgentApp(t, 80, 14)
	host.AppendMessage("user", "only one")
	app.renderSync(t)
	app.Frame()

	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyPgUp}, {Code: tea.KeyHome, Mod: tea.ModCtrl}} {
		press(t, app, key)
		if !app.scrolls["chat"].follow {
			t.Fatalf("short content cannot scroll: %s", key)
		}
	}
	if !strings.Contains(app.Frame(), "only one") {
		t.Fatalf("message must stay visible")
	}
}

// TestChatScrollAnchorsThroughRelayout: a narrower chat rewraps the lines
// above the viewport and ctrl+o expands the thoughts among them; both
// change how many rows sit above it, and it must keep showing the content
// it showed, not the row number it showed.
func TestChatScrollAnchorsThroughRelayout(t *testing.T) {
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
		appendReply(host, fmt.Sprintf("reply %d", i), hostapi.TurnStat{In: 1000, Out: 100, ElapsedMS: 1500})
	}
	app.renderSync(t)
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyPgUp})
	press(t, app, tea.KeyPressMsg{Code: tea.KeyPgUp})
	want := visible(app.Frame())

	app.Update(tea.WindowSizeMsg{Width: 40, Height: 14})
	app.renderSync(t)
	if got := visible(app.Frame()); got != want {
		t.Fatalf("resize moved the view from %q to %q", want, got)
	}

	host.ToggleReasoning()
	app.renderSync(t)
	if got := visible(app.Frame()); got != want {
		t.Fatalf("expanding the thoughts moved the view from %q to %q", want, got)
	}
}

// TestChatScrollAnchorsThroughFill: a long chat fills its latest rows first,
// and a reader who scrolled up in them keeps their place when the older rows
// land above, though every reply repeats the same lines.
func TestChatScrollAnchorsThroughFill(t *testing.T) {
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
	first := slices.IndexFunc(full, func(l []hostapi.Segment) bool { return strings.Contains(plainSegs(l), "message 28") })
	fill(full[first:], true)
	press(t, app, tea.KeyPressMsg{Code: tea.KeyPgUp})
	want, below := text(), app.rowsBelow()
	fill(full, false)
	if got := text(); got != want || app.rowsBelow() != below {
		t.Fatalf("the fill moved the view (%d rows below, want %d):\n%s\nwant:\n%s", app.rowsBelow(), below, got, want)
	}
}

// TestChatStickyPrompt: while a turn's rows fill the view, its prompt's
// first row stands in for the chat's top row, cut with an ellipsis when the
// prompt has more rows; it lights like a header, copies nothing, and a
// click or alt+up scrolls back to the prompt.
func TestChatStickyPrompt(t *testing.T) {
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
	row := func() string { return strings.TrimRight(plainSegs(app.lastRows[y][:len(app.lastRows[y])-1]), " ") }
	if !st.follow || !strings.HasPrefix(row(), "▌ fix the flaky") || !strings.HasSuffix(row(), "…") {
		t.Fatalf("following, the top row is %q", row())
	}

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
		if st.follow || !headerAt(st.lines, st.top) || st.first != nil || strings.Count(frame, "fix the flaky") != 1 {
			t.Fatalf("the jump left top %d (follow %v):\n%s", st.top, st.follow, frame)
		}
		press(t, app, tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModAlt})
		if app.Frame(); !st.follow {
			t.Fatalf("alt+down left top %d short of the tail", st.top)
		}
	}
}
