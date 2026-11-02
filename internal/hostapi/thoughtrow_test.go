package hostapi

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// TestThoughtRowGrowsInPlace: a reasoning chunk that lands after the call
// below it started grows the thought's row where it stands, so the epoch the
// chat drops the rows the reader opened by must not move, while the edit
// counter carries the grown text past its cache. A row landing before the
// next frame must not hide the chunk either.
func TestThoughtRowGrowsInPlace(t *testing.T) {
	for _, c := range []struct {
		name string
		row  bool // a row lands between the join and the render that reads it
	}{
		{"a bare join", false},
		{"a row after the join", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHostUI(t)
			hk := h.streamHooks()
			hk.ReasoningDelta("ponder")
			hk.ToolCallPending("t1", "grep")
			epoch, edit := h.trEpoch.Load(), h.trEdit.Load()
			hk.ReasoningDelta("ing.")
			if c.row {
				h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: "a notice"})
			}
			tr := h.Session().Transcript
			want := 1
			if c.row {
				want = 2
			}
			if len(tr) != want || tr[0].Kind != RowReasoning || tr[0].Text != "pondering." {
				t.Fatalf("rows = %v, want the thought grown in place", tr)
			}
			if h.trEpoch.Load() != epoch {
				t.Error("the joined chunk moved the epoch, so the chat drops the rows the reader opened")
			}
			if h.trEdit.Load() == edit {
				t.Error("the joined chunk left the edit counter, so the chat draws the grown row from its cache")
			}
		})
	}
}

// TestReplyReadoutDropsWhenTheNextReplyLands: the exchange the parent turn
// opens holds two replies. The parent turn holds on its background run (the
// end-of-turn note), the report lands in the same chain, and the answer behind
// it is another reply: the window reads both, and a rebuild reads the same
// rows - the readout that rides the last of them is the chat's to draw.
func TestReplyReadoutDropsWhenTheNextReplyLands(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	h.SetUIHooks(&fakeUI{})
	if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	answer := make(chan struct{})
	h.modelFn = func() (fantasy.LanguageModel, error) {
		return &scriptModel{script: func(system, last string, toolResult bool) []fantasy.StreamPart {
			switch {
			case strings.Contains(system, "a subagent of zemi"):
				<-release
				return textParts("R7REPORT ok")
			case toolResult:
				return textParts("started it")
			case strings.Contains(last, "<agent-report "):
				<-answer // the answer turn waits: the notice alone is on screen
				return textParts("the report said: R7REPORT ok")
			case strings.Contains(last, "Do not end the turn"):
				return toolCallParts("w1", "agent", `{"wait":"all"}`)
			default:
				return toolCallParts("c1", "agent", `{"description":"r7 readout","prompt":"reply R7REPORT ok","agent":"explore","background":true}`)
			}
		}}, nil
	}
	if err := h.AgentSend("go"); err != nil {
		t.Fatal(err)
	}
	// the turn holds on the run instead of ending into it
	waitFor(t, func() error {
		if n := h.tasks.runningFor(h.AgentSessionID()); n != 1 {
			return fmt.Errorf("running = %d, want 1", n)
		}
		return nil
	})
	close(release)
	// the report's notice can land while the step's reply still streams into
	// the transcript, and the exchange can move on to the report's answer
	// before this wait reads: the pin is that the notice left the reply in
	waitFor(t, func() error {
		var notice bool
		for _, m := range h.Session().Transcript {
			if m.Kind == "notice" && strings.Contains(m.Text, "R7REPORT ok") {
				notice = true
			}
		}
		last := lastText(h.Session().Transcript)
		if !notice || last != "started it" && last != "the report said: R7REPORT ok" {
			return fmt.Errorf("report = %v, exchange ends on %q, want its reply", notice, last)
		}
		return nil
	})
	// the notice alone changes nothing: the exchange's last text row is still
	// the reply it holds
	if last := lastText(h.Session().Transcript); last != "started it" && last != "the report said: R7REPORT ok" {
		t.Fatalf("the notice left the exchange ending on %q, want its reply", last)
	}
	close(answer)
	waitIdle(t, h)
	// the report steers into the turn that waits for it, or lands after it
	// ended: either way the answer behind the report is another reply, and a
	// resume reads the same rows the live window holds
	waitFor(t, func() error {
		if last := lastText(h.Session().Transcript); last != "the report said: R7REPORT ok" {
			return fmt.Errorf("the exchange ends on %q, want the report's answer", last)
		}
		return nil
	})
	waitIdle(t, h)
	live := shapes(h.Session().Transcript)
	h.setTranscriptFromLog()
	if rebuilt := shapes(h.Session().Transcript); !slices.Equal(live, rebuilt) {
		t.Fatalf("a resume reads other rows than the live window held:\nlive:\n%s\nrebuilt:\n%s",
			strings.Join(live, "\n"), strings.Join(rebuilt, "\n"))
	}
}

// lastText is the text of the window's last text row: the reply a readout
// would ride.
func lastText(tr []ChatMessage) string {
	for _, m := range slices.Backward(tr) {
		if m.Kind == "" && m.Text != "" {
			return m.Text
		}
	}
	return ""
}

// shapes is the rows as their role, kind and text - what a row reads as, its
// mark aside - enough to say a rebuild drew the rows the live window held.
func shapes(tr []ChatMessage) []string {
	out := make([]string, len(tr))
	for i, m := range tr {
		out[i] = fmt.Sprintf("%s/%s/%s", m.Role, m.Kind, m.Text)
	}
	return out
}
