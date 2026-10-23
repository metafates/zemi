package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/hostapi"
)

// The resumed-session tail: the last turn holds many runs and a long final
// reply; the reader is at the tail. Alt+m opens on the message the tail
// shows - the reply - not on the turn's prompt.
func TestMessagesResumedTailPicksTheReply(t *testing.T) {
	app, host := bootAgentApp(t, 120, 46)
	// three earlier turns
	host.AppendMessage("user", "earlier turn one")
	appendReply(host, "done one", hostapi.TurnStat{In: 1000, Out: 100, ElapsedMS: 1500})
	host.AppendMessage("user", "earlier turn two")
	appendReply(host, "done two", hostapi.TurnStat{In: 1000, Out: 100, ElapsedMS: 1500})
	// the last turn: a prompt, a run of tool calls, then a long reply
	host.AppendMessage("user", "the last prompt")
	for i := range 12 {
		host.AppendChat(hostapi.ChatMessage{Kind: "tool", Role: "tool", Tool: "shell", Label: fmt.Sprintf("step %d", i), Text: "ls", Status: hostapi.CallOK})
	}
	long := strings.Repeat("The audit found and fixed things. ", 30)
	appendReply(host, long, hostapi.TurnStat{In: 9000, Out: 300, ElapsedMS: 54_000})
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	if !chat.follow {
		t.Fatalf("the tail view should follow")
	}
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	if len(app.overlays) != 1 {
		t.Fatalf("%d overlays, want the list alone", len(app.overlays))
	}
	top := &app.overlays[0]
	// the reply's anchor: its transcript row is the last text row
	want := len(host.Session().Transcript)
	if got := top.selectedAnchor(); got != want {
		t.Fatalf("opened on anchor %d, want the reply %d", got, want)
	}
}
