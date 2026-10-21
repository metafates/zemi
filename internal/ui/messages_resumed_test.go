package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/hostapi"
)

// The resumed-session tail: the last turn holds many runs and a long final
// reply; the reader is at the tail. Alt+m must open on the message the tail
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
	// what the open's own levels leave behind: the turns list picks the last
	// turn and jumps the chat to its prompt (row 5); a level deriving its
	// selection from the moved chat opens on the prompt, not the reply
	chat.focus, chat.placed, chat.follow = 5, chat.top, false
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	var ids []string
	for _, st := range app.overlays {
		ids = append(ids, st.o.ID)
	}
	t.Logf("levels: %v", ids)
	if len(app.overlays) < 2 {
		t.Fatalf("%d overlays, want the turns list and the turn level", len(app.overlays))
	}
	top := &app.overlays[len(app.overlays)-1]
	if got := top.selectedAnchor(); got <= 0 {
		t.Fatalf("no selection")
	}
	// the reply's anchor: its transcript row is the last text row
	want := len(host.Session().Transcript)
	if got := top.selectedAnchor(); got != want {
		t.Fatalf("opened on anchor %d, want the reply %d; levels %v", got, want, ids)
	}
}
