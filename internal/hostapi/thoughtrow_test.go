package hostapi

import (
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
	// thought has not begun: both belong on the rail now
	got := widgetText(t, h, "chat", UIState{})
	if n := strings.Count(got, "✻ thought"); n != 2 {
		t.Fatalf("thought rows mid-turn = %d, want 2:\n%s", n, got)
	}
	unpark.Do(func() { close(release) })
	waitIdle(t, h)
}

// widgetText draws widget id and returns the text of its rows.
func widgetText(t *testing.T, h *Host, id string, state UIState) string {
	t.Helper()
	lines, _, err := h.RenderWidget(t.Context(), h.Widgets.Get(id), 80, 24, state)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(SegsText(l))
		b.WriteString("\n")
	}
	return b.String()
}

// TestThoughtRowGrowsInPlace: a reasoning chunk that lands after the call
// below it started grows the thought's row where it stands, so the epoch the
// chat widget drops the reader's opened rows by must not move: the row the
// reader opened keeps its place and draws the grown text, in the chat and in
// the thinking pane, which read the same rows. A row landing before the next
// render must not hide the chunk either.
func TestThoughtRowGrowsInPlace(t *testing.T) {
	for _, c := range []struct {
		name string
		row  bool // a row lands between the join and the render that reads it
	}{
		{"a bare join", false},
		{"a row after the join", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newTestHostUI(t)
			hk := h.streamHooks()
			hk.ReasoningDelta("ponder")
			hk.ToolCallPending("t1", "grep")
			pane := UIState{Regions: &map[string]bool{"thinking": true}}
			// the first renders settle each widget's cache
			widgetText(t, h, "chat", UIState{})
			widgetText(t, h, "thinking", pane)
			// the reader opens the thought row while the call runs, in the two
			// clicks the rail's rows take: its run, then the row itself
			for _, id := range []string{"turn:1", "thought:1"} {
				if err := h.ClickWidget(t.Context(), "chat", map[string]any{"id": id}); err != nil {
					t.Fatalf("click %s: %v", id, err)
				}
			}
			if got := widgetText(t, h, "chat", UIState{}); !strings.Contains(got, "ponder") {
				t.Fatalf("the opened thought draws no text:\n%s", got)
			}
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
				t.Error("the joined chunk moved the epoch, so the chat widget drops the rows the reader opened")
			}
			if h.trEdit.Load() == edit {
				t.Error("the joined chunk left the edit counter, so the widget draws the grown row from its cache")
			}
			if got := widgetText(t, h, "chat", UIState{}); !strings.Contains(got, "pondering.") {
				t.Fatalf("the joined chunk is not drawn:\n%s", got)
			}
			if got := widgetText(t, h, "thinking", pane); !strings.Contains(got, "pondering.") {
				t.Fatalf("the pane draws the thought as it was:\n%s", got)
			}
		})
	}
}
