package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/metafates/zemi/internal/hostapi"
)

// fillChat appends n user/reply pairs to the transcript.
func fillChat(t *testing.T, host *hostapi.Host, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
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
	fillChat(t, host, 30)
	app.renderSync(t)
	app.Frame()

	if !app.chatFollow {
		t.Fatalf("expected tail follow")
	}
	if !strings.Contains(app.Frame(), "reply 30") {
		t.Fatalf("tail view should show the latest reply")
	}

	press(t, app, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if app.chatFollow {
		t.Fatalf("pgup should leave tail follow")
	}
	if strings.Contains(app.Frame(), "reply 30") {
		t.Fatalf("scrolled view should not show the tail")
	}

	press(t, app, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if !app.chatFollow {
		t.Fatalf("pgdown into the bottom should re-arm follow")
	}
	if !strings.Contains(app.Frame(), "reply 30") {
		t.Fatalf("follow should show the tail again")
	}

	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	if app.chatFollow || app.chatTop != 0 {
		t.Fatalf("ctrl+home should pin the first row")
	}
	if !strings.Contains(app.Frame(), "message 1") {
		t.Fatalf("top view should show the first message")
	}

	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
	if !app.chatFollow {
		t.Fatalf("ctrl+end should re-arm follow")
	}
}

// TestChatScrollWheel pins the mouse wheel path: wheel up scrolls off the
// tail, wheel down re-arms follow at the bottom.
func TestChatScrollWheel(t *testing.T) {
	app, host := bootAgentApp(t, 80, 14)
	fillChat(t, host, 30)
	app.renderSync(t)
	app.Frame()

	app.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if app.chatFollow {
		t.Fatalf("wheel up should scroll off the tail")
	}
	for range 200 {
		app.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	}
	if !app.chatFollow {
		t.Fatalf("wheel down should re-arm follow at the bottom")
	}
}

// TestChatScrollStaysAnchored pins the reason scrollback exists: while the
// user is scrolled up, new transcript lines must not move the visible
// window (the tail keeps filling underneath).
func TestChatScrollStaysAnchored(t *testing.T) {
	app, host := bootAgentApp(t, 80, 14)
	fillChat(t, host, 30)
	app.renderSync(t)
	app.Frame()

	press(t, app, tea.KeyPressMsg{Code: tea.KeyPgUp})
	top := app.chatTop
	before := app.Frame()

	for i := 31; i <= 35; i++ {
		host.AppendMessage("user", fmt.Sprintf("message %d", i))
	}
	app.renderSync(t)
	after := app.Frame()

	if app.chatFollow {
		t.Fatalf("new lines must not yank a scrolled view")
	}
	if app.chatTop != top || after != before {
		t.Fatalf("scrolled frame should stay anchored as content grows")
	}
}

// TestChatScrollSubmitFollows pins the snap-back: sending a message returns
// the view to the tail.
func TestChatScrollSubmitFollows(t *testing.T) {
	app, host := bootAgentApp(t, 80, 14)
	fillChat(t, host, 30)
	app.renderSync(t)
	app.Frame()

	press(t, app, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if app.chatFollow {
		t.Fatalf("precondition: scrolled off the tail")
	}
	app.composer.text = "next question"
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !app.chatFollow {
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

	press(t, app, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if !app.chatFollow {
		t.Fatalf("short content cannot scroll")
	}
	if !strings.Contains(app.Frame(), "only one") {
		t.Fatalf("message must stay visible")
	}
}
