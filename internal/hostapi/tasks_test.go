package hostapi

import (
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

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
	if n := h.bgAgents.Load(); n != 1 {
		t.Fatalf("background agents running = %d, want 1", n)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		tr := h.Session().Transcript
		if last := tr[len(tr)-1]; last.Text == "the agent reported: found the thing" && !h.AgentActive() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("report never arrived: %+v", tr)
		}
		time.Sleep(10 * time.Millisecond)
	}
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
