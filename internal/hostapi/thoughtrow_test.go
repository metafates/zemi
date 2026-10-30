package hostapi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
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
	// thought has not begun: both belong on the rail now
	got := widgetText(t, h, "chat", 80, UIState{})
	if n := strings.Count(got, "✻ thought"); n != 2 {
		t.Fatalf("thought rows mid-turn = %d, want 2:\n%s", n, got)
	}
	unpark.Do(func() { close(release) })
	waitIdle(t, h)
}

// TestThoughtRowHoldsWhileReplying: when the reply starts, the thinking block
// folds to the digest row the thought settles as, and the settle that lands
// later draws the same rows, so the streaming reply never shifts. A thought
// the run above takes, and one too short to name a digest, draw nothing and
// let the settle place their rows.
func TestThoughtRowHoldsWhileReplying(t *testing.T) {
	for _, c := range []struct {
		name  string
		row   string // the digest row the reply streams under, "" for none
		drive func(hk agent.StreamHooks, clock *fakeClock)
	}{
		{"a thought", "· thought 3.0s", func(hk agent.StreamHooks, clock *fakeClock) {
			hk.ReasoningDelta("pondering the ask.")
			clock.set(testEpoch.Add(3 * time.Second))
			hk.TextDelta("The reply")
		}},
		{"whitespace only", "", func(hk agent.StreamHooks, clock *fakeClock) {
			hk.ReasoningDelta("   ")
			clock.set(testEpoch.Add(3 * time.Second))
			hk.TextDelta("The reply")
		}},
		{"a thought a call holds", "", func(hk agent.StreamHooks, clock *fakeClock) {
			hk.ReasoningDelta("first thought.")
			clock.set(testEpoch.Add(3 * time.Second))
			hk.ToolCallPending("t1", "glob")
			hk.ToolCallEnd("t1", "glob", out(""), "")
			hk.ReasoningDelta("second thought.")
			clock.set(testEpoch.Add(5 * time.Second))
			hk.TextDelta("The reply")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			clock := &fakeClock{now: testEpoch}
			h := newTestHostUI(t, clock)
			h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: "go"})
			h.agents.mu.Lock()
			h.agents.active = true // a turn runs: the run-above guard reads the live tail
			h.agents.mu.Unlock()
			c.drive(h.streamHooks(), clock)
			before := widgetText(t, h, "chat", 80, UIState{})
			if c.row == "" && strings.Contains(before, "\n· thought") {
				t.Fatalf("a digest row drew where the settle places none:\n%s", before)
			}
			if c.row != "" && !strings.Contains(before, c.row) {
				t.Fatalf("the thought's digest row is not drawn while the reply streams:\n%s", before)
			}
			h.agents.mu.Lock()
			h.flushOutput(TurnStat{})
			h.agents.mu.Unlock()
			after := widgetText(t, h, "chat", 80, UIState{})
			if a, b := strings.Count(after, "\n"), strings.Count(before, "\n"); a != b {
				t.Fatalf("the settle moved rows (%d -> %d):\nbefore:\n%s\nafter:\n%s", b, a, before, after)
			}
			if c.row != "" && after != before {
				t.Fatalf("the settle rewrote the rows:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// widgetText draws widget id and returns the text of its rows.
func widgetText(t *testing.T, h *Host, id string, width int, state UIState) string {
	t.Helper()
	lines, _, err := h.RenderWidget(t.Context(), h.Widgets.Get(id), width, 24, state)
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
// chat widget drops its cached units by must not move, while the edit counter
// makes it draw the grown row. A row landing before the next render must not
// hide the chunk either.
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
			// the first render settles the widget's cache
			if got := widgetText(t, h, "chat", 80, UIState{}); !strings.Contains(got, "thought") {
				t.Fatalf("the thought's row is not drawn:\n%s", got)
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
		})
	}
}

// TestViewedRunDrawsNoPendingRow: a run's own rows say what it is doing, so
// the chat draws none of the session turn's pending "thinking" row there -
// it would stand at the bottom of every running run.
func TestViewedRunDrawsNoPendingRow(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	h.SetUIHooks(&fakeUI{})
	def := AgentDef{Name: "explore"}
	run, _ := h.newRun(t.Context(), def, "look around", "look", true)
	if err := h.tasks.add(run); err != nil {
		t.Fatal(err)
	}
	if err := h.ViewAgent(run.id); err != nil {
		t.Fatal(err)
	}
	hk := &subagentHooks{h: h, def: def, run: run}
	hk.ToolCallStart("c1", "read", `{"path":"a.go"}`)
	hk.ToolCallEnd("c1", "read", out("package a"), "")
	// the run works on with nothing streaming, long enough for the turn's own
	// pending row to open
	run.mu.Lock()
	run.started = time.Now().Add(-2 * time.Second)
	run.mu.Unlock()
	if got := widgetText(t, h, "chat", 80, UIState{}); !strings.Contains(got, "read a.go") || strings.Contains(got, "thinking") {
		t.Fatalf("the run's view draws a pending row over its own rows:\n%s", got)
	}
}

// TestViewedRunThinksLive: a run the chat shows streams its thinking the way
// the session's turn does - the tail under a live thinking row - and the
// thought settles as one row where the call that ends it lands.
func TestViewedRunThinksLive(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	h.SetUIHooks(&fakeUI{})
	def := AgentDef{Name: "explore"}
	run, _ := h.newRun(t.Context(), def, "look around", "look", true)
	if err := h.tasks.add(run); err != nil {
		t.Fatal(err)
	}
	if err := h.ViewAgent(run.id); err != nil {
		t.Fatal(err)
	}
	hk := &subagentHooks{h: h, def: def, run: run}
	hk.ReasoningDelta("pondering the")
	hk.ReasoningDelta(" cause")
	got := widgetText(t, h, "chat", 80, UIState{})
	if !strings.Contains(got, "⠋ thinking") || !strings.Contains(got, "pondering the cause") {
		t.Fatalf("the run's live thinking tail is not drawn:\n%s", got)
	}
	if strings.Contains(got, "✻ thought") {
		t.Fatalf("the run's thought settled before the call that ends it:\n%s", got)
	}
	hk.ToolCallStart("c1", "read", `{"path":"a.go"}`)
	if rows := run.transcript(); len(rows) != 3 || rows[1].Kind != RowReasoning || rows[1].Text != "pondering the cause" || rows[2].Kind != RowTool {
		t.Fatalf("rows = %+v, want one thought above the call", run.transcript())
	}
	// a late chunk joins the thought the call left above, as the session's does
	hk.ReasoningDelta(" I see")
	if rows := run.transcript(); len(rows) != 3 || rows[1].Text != "pondering the cause I see" {
		t.Fatalf("rows = %+v, want the late chunk joined", run.transcript())
	}
	hk.ToolCallEnd("c1", "read", out("package a"), "")
	hk.TextDelta("it reads a.go")
	run.finish(h, nil)
	rows := run.transcript()
	if len(rows) != 4 || rows[2].Kind != RowTool || rows[3].Text != "it reads a.go" {
		t.Fatalf("rows = %+v, want the reply under the call", run.transcript())
	}
	// settled, the run folds into its digest: its one Thought row is behind it
	// rather than a live tail
	if got := widgetText(t, h, "chat", 80, UIState{}); !strings.Contains(got, "· 1 file") || strings.Contains(got, "thinking") {
		t.Fatalf("the settled run does not fold into its digest:\n%s", got)
	}
}

// TestReplyStatsShed: a settled reply's stats row leads with the model that
// wrote it and sheds it first, so a narrow chat keeps what the turn cost.
func TestReplyStatsShed(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	h.AppendChat(ChatMessage{
		Role: fantasy.MessageRoleAssistant, Text: "done",
		Stat: &TurnStat{Out: 100, ElapsedMS: 1500, Model: "anthropic/claude-sonnet-4-5"},
	})
	wide := widgetText(t, h, "chat", 80, UIState{})
	if !strings.Contains(wide, "anthropic/claude-sonnet-4-5 · 1.5s · 100 out") {
		t.Fatalf("the wide stats row does not lead with the model:\n%s", wide)
	}
	narrow := widgetText(t, h, "chat", 40, UIState{})
	if strings.Contains(narrow, "anthropic") || !strings.Contains(narrow, "1.5s · 100 out") {
		t.Fatalf("the narrow stats row kept the model or lost the turn:\n%s", narrow)
	}
}

// TestReplyReadoutDropsWhenTheNextReplyLands: the readout rides the reply
// that ends the exchange. The parent turn holds on its background run (the
// end-of-turn note), the report lands in the same chain, and the answer
// behind it is another reply, so the readout drops live the way a rebuild
// draws it - a resume reads the same lines.
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
			}
			return toolCallParts("c1", "agent", `{"description":"r7 readout","prompt":"reply R7REPORT ok","agent":"explore","background":true}`)
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
	waitFor(t, func() error {
		for _, m := range h.Session().Transcript {
			if m.Kind == "notice" && strings.Contains(m.Text, "R7REPORT ok") {
				return nil
			}
		}
		return fmt.Errorf("report never arrived: %+v", h.Session().Transcript)
	})
	// the notice alone changes nothing: the readout stays
	mid := widgetText(t, h, "chat", 100, UIState{})
	if !readoutUnder(t, mid, "started it") {
		t.Fatalf("the notice dropped the readout:\n%s", mid)
	}
	close(answer)
	waitIdle(t, h)
	// the answer behind the report is another reply: the readout drops from
	// the exchange's first reply and rides the last, and a rebuild draws the
	// same lines
	live := widgetText(t, h, "chat", 100, UIState{})
	h.setTranscriptFromLog()
	rebuilt := widgetText(t, h, "chat", 100, UIState{})
	if live != rebuilt {
		t.Fatalf("a resume reads other lines than the live window drew:\nlive:\n%s\nrebuilt:\n%s", live, rebuilt)
	}
	if readoutUnder(t, live, "started it") {
		t.Fatalf("the readout stayed under a reply another reply follows:\n%s", live)
	}
	if !readoutUnder(t, live, "the report said") {
		t.Fatalf("the final reply drew no readout:\n%s", live)
	}
}

// readoutUnder says the first row after name's row draws a readout: what a
// turn's readout ends in, the clock.
func readoutUnder(t *testing.T, frame, name string) bool {
	t.Helper()
	lines := strings.Split(frame, "\n")
	for i, l := range lines {
		if !strings.Contains(l, name) {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if s := strings.TrimSpace(lines[j]); s != "" {
				return endsWithClock(s)
			}
		}
	}
	return false
}

// endsWithClock says the row's last field is an HH:MM clock, the readout's
// last part.
func endsWithClock(s string) bool {
	if len(s) < 5 {
		return false
	}
	c := s[len(s)-5:]
	d := func(b byte) bool { return b >= '0' && b <= '9' }
	return c[2] == ':' && d(c[0]) && d(c[1]) && d(c[3]) && d(c[4])
}
