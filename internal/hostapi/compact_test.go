package hostapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

// TestContextEstimateCountsPrompt: the estimate a compaction installs counts
// the system prompt and tool schemas every prompt carries, not only the
// messages that survived.
func TestContextEstimateCountsPrompt(t *testing.T) {
	h, _ := newTestHostDB(t)
	reflectStub(t, h, "ok")
	h.reflectMu.Lock()
	defer h.reflectMu.Unlock()
	sender(t, h)("hi")
	h.agents.mu.Lock()
	runner, overhead := h.agents.runner, h.agents.promptTokens
	h.agents.mu.Unlock()
	if overhead <= 0 {
		t.Fatal("the runner's prompt was never measured")
	}
	if err := h.compact(context.Background(), runner, compactManual, ""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.agents.mu.Lock()
	got, want := h.agents.lastIn, estTokens(runner.History...)+overhead
	h.agents.mu.Unlock()
	if got != want {
		t.Fatalf("estimate %d, want %d (messages plus the prompt)", got, want)
	}
}

// TestCompactModelSetting: compaction summaries can run on their own model.
func TestCompactModelSetting(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.provider.register({ id = "p", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1",
			models = { { id = "sum" } } })
		zemi.config.set("compact.model", "p/sum")
	`)
	m, err := h.modelFor("compact.model")
	if err != nil || m.Model() != "sum" {
		t.Fatalf("summarizer model = %v, err %v", m, err)
	}
}

// TestSummaryKeptWhenCutOff: a summary the model cut at the output limit is
// used with a note, instead of dropping the history unsummarized.
func TestSummaryKeptWhenCutOff(t *testing.T) {
	h, _ := newTestHostDB(t)
	m := reflectStub(t, h, "## Goal\nship it")
	m.cutReply = true
	out, err := h.summarize(context.Background(), []fantasy.Message{textMessage("hi")}, "", "", 0)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if !strings.HasPrefix(out, "## Goal") || !strings.Contains(out, "cut off") {
		t.Fatalf("summary = %q", out)
	}
}

// TestCompactNowCancels: esc during /compact stops the summary call instead
// of leaving the agent busy until the model answers.
func TestCompactNowCancels(t *testing.T) {
	h, _ := newTestHostDB(t)
	m := reflectStub(t, h, "") // an empty reply starts no title call
	m.block = time.Minute
	h.reflectMu.Lock()
	defer h.reflectMu.Unlock()
	sender(t, h)("hi")
	if err := h.CompactNow(""); err != nil {
		t.Fatal(err)
	}
	h.AgentCancel()
	waitIdle(t, h)
}

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
		out, freed := pruneToolOutput(history, false)
		if freed != c.wantFreed {
			t.Errorf("%s: freed %d, want %d", c.name, freed, c.wantFreed)
		}
		cleared := toolText(out[0].Content[0].(fantasy.ToolResultPart)) == clearedOutput
		if cleared != (c.wantFreed > 0) || estTokens(out[1]) != toolKeepTokens || estTokens(history[0]) != int64(c.oldTokens) {
			t.Errorf("%s: cleared %v, newest %d tokens, input %d tokens", c.name, cleared, estTokens(out[1]), estTokens(history[0]))
		}
	}
}

// TestPruneKeepsRunningTurn: pruning between the steps of one turn must
// leave the output after the last user message, which the turn continues
// from, even when it is past the kept budget.
func TestPruneKeepsRunningTurn(t *testing.T) {
	history := []fantasy.Message{
		textMessage("old ask"), toolResult("old", 30_000),
		textMessage("live ask"), toolResult("live", 50_000),
	}
	out, freed := pruneToolOutput(history, false)
	if freed != 30_000 {
		t.Fatalf("freed %d tokens, want the old 30000", freed)
	}
	if toolText(out[1].Content[0].(fantasy.ToolResultPart)) != clearedOutput {
		t.Fatal("old tool output must be cleared")
	}
	if estTokens(out[3]) != 50_000 {
		t.Fatalf("the running turn's output was pruned to %d tokens", estTokens(out[3]))
	}
}

// TestPruneInTurnClearsSubagentOutput: a subagent's whole run is one turn,
// so clearing its old output cannot wait for a user message that never comes.
func TestPruneInTurnClearsSubagentOutput(t *testing.T) {
	history := []fantasy.Message{textMessage("look"), toolResult("old", 30_000), toolResult("new", toolKeepTokens)}
	if _, freed := pruneToolOutput(history, false); freed != 0 {
		t.Fatalf("a running turn's output was cleared: %d tokens", freed)
	}
	out, freed := pruneToolOutput(history, true)
	if freed != 30_000 || toolText(out[1].Content[0].(fantasy.ToolResultPart)) != clearedOutput {
		t.Fatalf("freed %d tokens, old output %q", freed, toolText(out[1].Content[0].(fantasy.ToolResultPart)))
	}
	if estTokens(out[2]) != toolKeepTokens {
		t.Fatalf("the newest output was cleared too: %d tokens", estTokens(out[2]))
	}
}

// TestTranscriptTextNotesAttachments: a compacted transcript names attached
// files, which the summarizer cannot see, instead of dropping them silently.
func TestTranscriptTextNotesAttachments(t *testing.T) {
	got := transcriptText([]fantasy.Message{{
		Role: fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "what is wrong here"},
			fantasy.FilePart{MediaType: "image/png", Data: []byte("x")},
		},
	}})
	if !strings.Contains(got, "what is wrong here") || !strings.Contains(got, "[attached image/png]") {
		t.Fatalf("transcript = %q", got)
	}
}

func toolResult(id string, tokens int) fantasy.Message {
	return fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
		fantasy.ToolResultPart{ToolCallID: id, Output: fantasy.ToolResultOutputContentText{Text: strings.Repeat("r", tokens*4)}},
	}}
}

// TestCapRefusal: a refused output cap is dropped and resent without an
// error row, and Anthropic's context overflow, which names max_tokens too,
// is compacted instead and keeps the cap.
func TestCapRefusal(t *testing.T) {
	for _, c := range []struct {
		name    string
		msg     string
		dropped bool
	}{
		{"cap refused", "max_tokens: 8000 > 4096, which is the maximum allowed", true},
		{"context overflow", "input length and `max_tokens` exceed context limit: 195000 + 8000 > 200000, decrease input length or `max_tokens`", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, r := newTestHostDB(t)
			runUser(t, r, `zemi.provider.register({ id = "a", kind = "anthropic", models = { { id = "m", max_output = 8000 } } })`)
			t.Cleanup(func() { capRejected.Delete("a/m") })
			m := reflectStub(t, h, "ok")
			m.failOnce.Store(&fantasy.ProviderError{StatusCode: 400, Message: c.msg})
			h.reflectMu.Lock()
			defer h.reflectMu.Unlock()
			sender(t, h)("hi")
			if n := m.streams.Load(); n != 2 {
				t.Fatalf("model streamed %d times, want 2", n)
			}
			if _, dropped := capRejected.Load("a/m"); dropped != c.dropped {
				t.Fatalf("cap dropped: %v, want %v", dropped, c.dropped)
			}
			for _, row := range h.Session().Transcript {
				if row.Kind == "error" {
					t.Fatalf("error row shown: %+v", row)
				}
			}
		})
	}
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
