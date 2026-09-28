package hostapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// TestPanicsDoNotKillZemi: a panicking host goroutine is recovered into
// /errors, and a turn that panics leaves the agent idle, not stuck active.
func TestPanicsDoNotKillZemi(t *testing.T) {
	h, _ := newTestHost(t)
	done := make(chan struct{})
	h.GoSafe("test", func() {
		defer close(done)
		panic("boom")
	})
	<-done
	waitFor(t, func() error {
		if !h.HasUnseenErrors() {
			return errors.New("panic not recorded")
		}
		return nil
	})
	if !strings.Contains(h.errs.entries[0].msg, "boom") {
		t.Fatalf("panic not recorded: %+v", h.errs.entries)
	}

	h.agents.mu.Lock()
	h.agents.active = true
	h.agents.mu.Unlock()
	func() {
		defer h.endTurnAfterPanic()
		panic("turn exploded")
	}()
	if h.AgentActive() {
		t.Fatal("agent still active after a turn panic")
	}
}

// TestFailedTurnLeavesErrorRow: a turn that fails ends with an error row in
// the transcript (and the agent idle); an interrupt reads as interrupted.
// Reasoning mid-step settles the finished call and keeps the open one live,
// which the end then marks failed, or stopped after an interrupt.
func TestFailedTurnLeavesErrorRow(t *testing.T) {
	h, _ := newTestHost(t)
	sa := &streamAdapter{h: h}
	for _, c := range []struct {
		err          error
		text, status string
		call         string
	}{
		{errors.New("dial tcp: no route to host"), "Dial tcp: no route to host", "", "err"},
		{context.Canceled, "Interrupted", "cancelled", "stopped"},
	} {
		sa.ToolCallStart("a", "glob", `{"pattern":"*"}`)
		sa.ToolCallEnd("a", "glob", true, "x.go", "")
		sa.ToolCallPending("b", "read")
		sa.ReasoningDelta("then b")
		if tr := h.Session().Transcript; tr[len(tr)-1].Status != "ok" || len(h.agents.tools) != 1 {
			t.Fatalf("%v: reasoning settled an open call: live %+v", c.err, h.agents.tools)
		}
		sa.TurnDone(c.err, fantasy.Usage{})
		tr := h.Session().Transcript
		last := tr[len(tr)-1]
		if last.Kind != "error" || last.Text != c.text || string(last.Status) != c.status {
			t.Fatalf("%v: last row = %+v", c.err, last)
		}
		if call := tr[len(tr)-2]; call.Tool != "read" || string(call.Status) != c.call {
			t.Fatalf("%v: open call = %+v", c.err, call)
		}
	}
}

// TestBuiltinSkillsShipEnabled: zemi's self-knowledge skill is written to
// the data dir, discovered as builtin and enabled by default.
func TestBuiltinSkillsShipEnabled(t *testing.T) {
	h, _ := newTestHostSkills(t)
	h.syncSkills()
	s, ok := h.skillsReg.get("zemi")
	if !ok || s.Source != "builtin" || !h.skillsReg.isEnabled("zemi") {
		t.Errorf("zemi: found=%v source=%q enabled=%v", ok, s.Source, h.skillsReg.isEnabled("zemi"))
	}
	if !strings.Contains(h.skillsIndexPrompt(), "- zemi: ") {
		t.Fatal("the builtin skill is missing from the prompt index")
	}
}
