package ui

import (
	"fmt"
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
		host.AppendTurnResult(fmt.Sprintf("reply %d", i), hostapi.TurnStat{In: 1000, Out: 100, ElapsedMS: 1500})
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

func withoutStatus(frame string) string {
	rows := strings.Split(frame, "\r\n")
	return strings.Join(rows[:len(rows)-1], "\r\n")
}

// TestChatScrollSubmitFollows pins the snap-back: sending a message returns
// the view to the tail.
func TestChatScrollSubmitFollows(t *testing.T) {
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame()

	press(t, app, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if app.scrolls["chat"].follow {
		t.Fatalf("precondition: scrolled off the tail")
	}
	app.composer.text = "next question"
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !app.scrolls["chat"].follow {
		t.Fatalf("submit should snap back to the tail")
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
		for _, row := range strings.Split(frame, "\r\n") {
			if i := strings.Index(ansi.Strip(row), "message "); i >= 0 {
				return strings.TrimSpace(ansi.Strip(row)[i:])
			}
		}
		t.Fatalf("no message row on screen:\n%s", frame)
		return ""
	}
	app, host := bootAgentApp(t, 100, 14)
	for i := 1; i <= 20; i++ {
		host.AppendMessage("user", fmt.Sprintf("message %d", i))
		host.AppendReasoning(fmt.Sprintf("thought %d line one\nthought %d line two\n", i, i), hostapi.TurnStat{ThoughtMS: 4200})
		host.AppendTurnResult(fmt.Sprintf("reply %d", i), hostapi.TurnStat{In: 1000, Out: 100, ElapsedMS: 1500})
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
