package hostapi

import (
	"runtime"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/tools"
)

// TestAskFIFO: asks reach the user one at a time under askMu, and the user's
// tell denies the ones waiting behind the prompt.
func TestAskFIFO(t *testing.T) {
	for _, c := range []struct {
		name          string
		first, second tools.Request
		by            string        // the first prompt's answer
		during        func(h *Host) // what happens while it is open
		prompts       int
		allowed       [2]bool
		secondBy      string
	}{
		{
			"an answer does not cover the waiter",
			tools.Request{Subject: "tool.shell", Arg: "make out"},
			tools.Request{Subject: "tool.shell", Arg: "make test"},
			"user:once", nil, 2,
			[2]bool{true, true},
			"user:once",
		},
		{
			"a tell denies the waiter",
			tools.Request{Subject: "tool.shell", Arg: "make out"},
			tools.Request{Subject: "tool.shell", Arg: "make test"},
			tools.ByUser, func(h *Host) { h.DenyWaitingAsks() }, 1,
			[2]bool{false, false},
			tools.ByUser,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestHost(t)
			opened := make(chan struct{})
			ui := &fakeUI{}
			ui.onAsk = func(PermRequest) (bool, string) {
				if len(ui.asked) == 1 {
					close(opened)
					// the second is at the prompt stage; t.Error, since this is no test goroutine
					for deadline := time.Now().Add(3 * time.Second); h.AsksWaiting() < 2; runtime.Gosched() {
						if time.Now().After(deadline) {
							t.Error("the second ask never reached the prompt stage")
							break
						}
					}
					if c.during != nil {
						c.during(h)
					}
				}
				return c.by != tools.ByUser, c.by
			}
			h.SetUIHooks(ui)
			var first tools.Verdict
			done := make(chan struct{})
			go func() {
				allowed, by := h.ask(c.first)
				first = tools.Verdict{Allowed: allowed, DecidedBy: by}
				close(done)
			}()
			<-opened
			allowed, secondBy := h.ask(c.second)
			second := tools.Verdict{Allowed: allowed, DecidedBy: secondBy}
			<-done
			if got := [2]bool{first.Allowed, second.Allowed}; got != c.allowed || len(ui.asked) != c.prompts || second.DecidedBy != c.secondBy {
				t.Fatalf("allowed %v, %d prompts, second by %q", got, len(ui.asked), second.DecidedBy)
			}
		})
	}
}
