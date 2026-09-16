package hostapi

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

// TestTaskRegEviction: a full registry forgets its oldest finished run, never
// the one being added, however old that one is.
func TestTaskRegEviction(t *testing.T) {
	reg := newTaskReg()
	for i := range maxKeptRuns {
		reg.add(&agentRun{id: fmt.Sprint(i), status: "done", started: time.Now()})
	}
	old := &agentRun{id: "old", status: "done", started: time.Now().Add(-time.Hour)}
	reg.add(old)
	if reg.find("old") != old || len(reg.runs) != maxKeptRuns {
		t.Fatalf("added run evicted, or nothing was: %d runs", len(reg.runs))
	}
}

// TestBackgroundAgent: the parent turn launches a slow background agent and
// finishes; the report then arrives as a message that starts a new turn,
// shown in the transcript as a one-line notice.
func TestBackgroundAgent(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, last string, toolResult bool) []fantasy.StreamPart {
			switch {
			case strings.Contains(system, "a subagent of zemi"):
				time.Sleep(150 * time.Millisecond)
				return textParts("found the thing")
			case toolResult:
				return textParts("started it")
			case strings.HasPrefix(last, "Background agent"):
				return textParts("the agent reported: " + last[strings.Index(last, "found"):strings.Index(last, "found")+15])
			}
			return toolCallParts("c1", "agent", `{"description":"slow search","prompt":"find it","agent":"explore","background":true}`)
		}}, nil
	}
	if err := h.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	if n := h.tasks.running(); n != 1 {
		t.Fatalf("background agents running = %d, want 1", n)
	}
	waitFor(t, func() error {
		if tr := h.Session().Transcript; tr[len(tr)-1].Text != "the agent reported: found the thing" || h.AgentActive() {
			return fmt.Errorf("report never arrived: %+v", tr)
		}
		return nil
	})
	notice := false
	for _, m := range h.Session().Transcript {
		if m.Role == "user" && strings.HasPrefix(m.Text, "Background agent ") {
			notice = !strings.Contains(m.Text, "found the thing")
		}
	}
	if !notice {
		t.Fatalf("transcript should show a one-line notice: %+v", h.Session().Transcript)
	}
}
