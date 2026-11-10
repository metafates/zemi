package hostapi

import (
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
)

// TestThoughtRowLandsWhenItsCallStarts: the thought the call after it closes
// is a row of the rail while the turn runs, not one step later with the reply.
func TestThoughtRowLandsWhenItsCallStarts(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	var calls atomic.Int64
	var park, unpark sync.Once
	reached, release := make(chan struct{}), make(chan struct{})
	// a failure before the release must not leave the third call parked
	defer unpark.Do(func() { close(release) })
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(_, _ string, _ bool) []fantasy.StreamPart {
			switch calls.Add(1) {
			case 1:
				return append(reasoningParts("first"), toolCallParts("c1", "glob", `{"pattern":"*.go"}`)...)
			case 2:
				return append(reasoningParts("second"), toolCallParts("c2", "glob", `{"pattern":"*.go"}`)...)
			}
			park.Do(func() { close(reached) })
			<-release
			return textParts("done")
		}}, nil
	}
	if err := h.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the third model call did not start")
	}
	// the second thought ended with the call that follows it, and the step's own
	// thought has not begun: both are rows of the window now
	var thoughts []string
	for _, m := range h.Session().Transcript {
		if m.Kind == RowReasoning {
			thoughts = append(thoughts, m.Text)
		}
	}
	if !slices.Equal(thoughts, []string{"first", "second"}) {
		t.Fatalf("thought rows mid-turn = %v, want both, the second one with the call that ended it", thoughts)
	}
	unpark.Do(func() { close(release) })
	waitIdle(t, h)
}
