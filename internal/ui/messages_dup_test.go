package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
)

// TestMessagesRowsKeyedByRow: a rail of calls that read alike keeps its
// selection when the transcript grows under it, because a row is named by the
// transcript row it draws, not by its label.
func TestMessagesRowsKeyedByRow(t *testing.T) {
	app, host := messagesApp(t)
	host.AppendMessage("user", "walk the rail")
	for i := 1; i <= 3; i++ {
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "shell", Text: "go test ./...", Label: "run the tests", Status: "ok"})
	}
	appendReply(host, "all green", hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	pump := func() {
		for range 3 {
			for app.Pump() {
			}
			app.Frame()
			app.renderSync(t)
		}
		app.Frame()
	}
	top := func() *overlayState { return &app.overlays[len(app.overlays)-1] }
	if err := host.MessagesCommand("turn 1"); err != nil {
		t.Fatal(err)
	}
	pump()
	press(t, app, tea.KeyPressMsg{Code: 'k', Text: "k"})
	pump()
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	pump()
	press(t, app, tea.KeyPressMsg{Code: 'j', Text: "j"})
	press(t, app, tea.KeyPressMsg{Code: 'j', Text: "j"})
	pump()
	// the transcript grows: the level rebuilds and must keep its row
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "notice", Text: "a notice"})
	pump()
	if got := top().selectedAnchor(); got != 4 {
		t.Fatalf("the selection moved to anchor %d, want 4 (the third call)", got)
	}
}
