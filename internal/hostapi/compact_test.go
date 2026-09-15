package hostapi

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// TestCompaction: a context past the threshold summarizes the history
// before the next turn while the recent messages stay verbatim, the summary
// becomes the session's resume point, and /resume rebuilds summary + tail
// without showing the tail twice.
func TestCompaction(t *testing.T) {
	h, _ := newTestHostDB(t)
	m := reflectStub(t, h, "ok")
	h.reflectMu.Lock() // no detached reflect racing the DB teardown
	defer h.reflectMu.Unlock()
	send := sender(t, h)

	send(strings.Repeat("x", 80_000)) // 20k tokens: past the 15k tail budget
	first := h.AgentSessionID()
	h.agents.mu.Lock()
	h.agents.ctxWin, h.agents.lastIn = 100_000, 90_000
	h.agents.mu.Unlock()
	send("two")
	if n := m.lastPrompt.Load(); n != 4 {
		t.Fatalf("compacted prompt has %d messages, want 4 (system, summary, ok, two)", n)
	}
	compacted := false
	for _, c := range h.Session().Transcript {
		compacted = compacted || c.Kind == "compact"
	}
	if !compacted {
		t.Fatal("transcript misses the compaction row")
	}
	if sess, err := h.DB.GetSession(first); err != nil || sess.SummaryMessageID == "" {
		t.Fatalf("summary pointer: %+v %v", sess, err)
	}

	if err := h.NewSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.ResumeSession(first); err != nil {
		t.Fatal(err)
	}
	oks := 0
	for _, c := range h.Session().Transcript {
		if c.Text == "ok" {
			oks++
		}
	}
	if oks != 2 {
		t.Fatalf("resumed transcript shows %d replies, want 2", oks)
	}
	send("three")
	if n := m.lastPrompt.Load(); n != 6 {
		t.Fatalf("resumed prompt has %d messages, want 6 (system, summary, ok, two, ok, three)", n)
	}
}

// TestCompactionMidTurn: a step that fills the context cuts the turn before
// the model reads its tool result; the history is compacted and the same
// turn continues from the summary.
func TestCompactionMidTurn(t *testing.T) {
	h, _ := newTestHostDB(t)
	m := reflectStub(t, h, "ok")
	h.reflectMu.Lock()
	defer h.reflectMu.Unlock()
	h.agents.mu.Lock()
	h.agents.ctxWin = 100_000
	h.agents.mu.Unlock()
	m.toolStep.Store(90_000)
	sender(t, h)("go")
	if n := m.streams.Load(); n != 2 {
		t.Fatalf("model streamed %d times, want 2", n)
	}
	if n := m.lastPrompt.Load(); n != 2 {
		t.Fatalf("continued prompt has %d messages, want 2 (system, summary)", n)
	}
	var kinds []string
	for _, row := range h.Session().Transcript {
		kinds = append(kinds, row.Kind)
	}
	if got := strings.Join(kinds, ","); !strings.Contains(got, "compact") || strings.Contains(got, "error") {
		t.Fatalf("transcript kinds: %s", got)
	}
}

// TestOverflowRecovery: a prompt the provider refuses as too long is
// compacted and resent once without a second copy of the user message; a
// second refusal is reported, not retried.
func TestOverflowRecovery(t *testing.T) {
	for _, c := range []struct {
		name     string
		refusals int32
		wantErr  bool
	}{
		{"recovers", 1, false},
		{"reports the second refusal", 2, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			m := reflectStub(t, h, "ok")
			m.overflow.Store(c.refusals)
			h.reflectMu.Lock()
			defer h.reflectMu.Unlock()
			sender(t, h)("hi")
			if n := m.streams.Load(); n != 2 {
				t.Fatalf("model streamed %d times, want 2", n)
			}
			if n := m.lastPrompt.Load(); n != 2 {
				t.Fatalf("resent prompt has %d messages, want 2 (system, summary)", n)
			}
			errRow := false
			for _, row := range h.Session().Transcript {
				errRow = errRow || row.Kind == "error"
			}
			if errRow != c.wantErr {
				t.Fatalf("error row shown: %v, want %v", errRow, c.wantErr)
			}
		})
	}
}

// TestCompactionStopsAfterFailures: auto compaction gives up for the
// session after compactMaxFails failures in a row.
func TestCompactionStopsAfterFailures(t *testing.T) {
	h, _ := newTestHostDB(t)
	m := reflectStub(t, h, "") // an empty reply starts no title call
	m.err = errors.New("summarizer down")
	h.reflectMu.Lock()
	defer h.reflectMu.Unlock()
	send := sender(t, h)
	send("warm up")
	for i := range compactMaxFails + 1 {
		h.agents.mu.Lock()
		h.agents.ctxWin, h.agents.lastIn = 100_000, 90_000
		h.agents.mu.Unlock()
		send(fmt.Sprint(i))
	}
	if n := m.calls.Load(); n != compactMaxFails {
		t.Fatalf("summarizer called %d times, want %d", n, compactMaxFails)
	}
}

func TestTailStart(t *testing.T) {
	user := func(bytes int) fantasy.Message { return textMessage(strings.Repeat("u", bytes)) }
	call := func(bytes int) fantasy.Message {
		return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "c", ToolName: "read", Input: strings.Repeat("i", bytes)},
		}}
	}
	result := toolResult("c", 10)
	for _, c := range []struct {
		name    string
		history []fantasy.Message
		want    int
	}{
		{"all fits", []fantasy.Message{user(40), call(8), result}, 0},
		{"cuts before a user message", []fantasy.Message{user(4000), user(40), call(8), result}, 1},
		{"a tool result never starts the tail", []fantasy.Message{user(40), call(4000), result}, 3},
	} {
		if got := tailStart(c.history, 100); got != c.want {
			t.Errorf("%s: tail starts at %d, want %d", c.name, got, c.want)
		}
	}
}

func TestPruneToolOutput(t *testing.T) {
	for _, c := range []struct {
		name      string
		oldTokens int
		wantFreed int64
	}{
		{"a small saving clears nothing", 10_000, 0},
		{"old output past the kept 40k is cleared", 30_000, 30_000},
	} {
		history := []fantasy.Message{toolResult("old", c.oldTokens), toolResult("new", toolKeepTokens)}
		out, freed := pruneToolOutput(history)
		if freed != c.wantFreed {
			t.Errorf("%s: freed %d, want %d", c.name, freed, c.wantFreed)
		}
		cleared := toolText(out[0].Content[0].(fantasy.ToolResultPart)) == clearedOutput
		if cleared != (c.wantFreed > 0) || estTokens(out[1]) != toolKeepTokens || estTokens(history[0]) != int64(c.oldTokens) {
			t.Errorf("%s: cleared %v, newest %d tokens, input %d tokens", c.name, cleared, estTokens(out[1]), estTokens(history[0]))
		}
	}
}

func toolResult(id string, tokens int) fantasy.Message {
	return fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
		fantasy.ToolResultPart{ToolCallID: id, Output: fantasy.ToolResultOutputContentText{Text: strings.Repeat("r", tokens*4)}},
	}}
}

func sender(t *testing.T, h *Host) func(string) {
	return func(text string) {
		t.Helper()
		if err := h.AgentSend(text); err != nil {
			t.Fatalf("send %q: %v", text, err)
		}
		waitIdle(t, h)
	}
}
