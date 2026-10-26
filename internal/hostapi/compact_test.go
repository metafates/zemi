package hostapi

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/store"
)

// TestContextEstimateCountsPrompt: the estimate a compaction installs counts
// the system prompt and tool schemas every prompt carries, not only the
// messages that survived.
func TestContextEstimateCountsPrompt(t *testing.T) {
	t.Parallel()
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
	if err := h.compact(t.Context(), runner, compactManual, ""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.agents.mu.Lock()
	got, want := h.agents.lastIn, estTokens(runner.History...)+overhead
	h.agents.mu.Unlock()
	if got != want {
		t.Fatalf("estimate %d, want %d (messages plus the prompt)", got, want)
	}
}

// TestSummaryKeptWhenCutOff: a summary the model cut at the output limit is
// used with a note, instead of dropping the history unsummarized.
func TestSummaryKeptWhenCutOff(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	m := reflectStub(t, h, "## Goal\nship it")
	m.cutReply = true
	out, err := h.summarize(t.Context(), []fantasy.Message{agent.UserMessage("hi")}, "", "", 0)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if !strings.HasPrefix(out, "## Goal") || !strings.Contains(out, "cut off") {
		t.Fatalf("summary = %q", out)
	}
	if got := h.AgentStream().SessionOut; got != 7 {
		t.Fatalf("the summary call billed %d output tokens to the session, want 7", got)
	}
}

// TestCompactNowCancels: esc during an idle compaction stops the summary
// call instead of leaving the agent busy until the model answers, and leaves
// the trace any user interrupt leaves: an error row marked cancelled in the
// transcript, and the cancellation in the log.
func TestCompactNowCancels(t *testing.T) {
	for _, c := range []struct {
		name   string
		reason string
		run    func(*Host)
	}{
		{"manual /compact", "manual", func(h *Host) { h.CompactNow("") }},
		{"idle auto compact", "auto", func(h *Host) { h.AutoCompact() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestHostDB(t)
			m := reflectStub(t, h, "") // an empty reply starts no title call
			m.block.Store(int64(time.Minute))
			h.reflectMu.Lock()
			defer h.reflectMu.Unlock()
			sender(t, h)("hi")
			var log bytes.Buffer
			h.Log = slog.New(slog.NewTextHandler(&log, nil))
			c.run(h)
			h.AgentCancel()
			waitIdle(t, h)
			row, status := "", CallStatus("")
			for _, r := range h.Session().Transcript {
				if r.Kind == RowError {
					row, status = r.Text, r.Status
				}
			}
			if row != "Interrupted" || status != CallCancelled {
				t.Fatalf("no interrupted row for the cancelled compact: %q %q", row, status)
			}
			if line := log.String(); !strings.Contains(line, "compaction cancelled") || !strings.Contains(line, "reason="+c.reason) {
				t.Fatalf("the log does not name the cancellation: %s", line)
			}
		})
	}
}

// TestAutoCompact: the idle timer's compact runs like /compact when the
// session has history, and sits silent on a fresh one.
func TestAutoCompact(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	reflectStub(t, h, "the user asked for a build fix; it passed")
	h.reflectMu.Lock()
	defer h.reflectMu.Unlock()
	sender(t, h)("hi")
	h.AutoCompact()
	waitIdle(t, h)
	tr := h.Session().Transcript
	if last := tr[len(tr)-1]; last.Kind != RowCompact {
		t.Fatalf("last row %v, want the compaction's summary row", last.Kind)
	}

	h2, _ := newTestHostDB(t)
	reflectStub(t, h2, "should never be asked")
	h2.AutoCompact()
	if tr := h2.Session().Transcript; len(tr) != 0 {
		t.Fatalf("a fresh session compacted: %d rows", len(tr))
	}
}

// TestCompaction: a history past the threshold summarizes into the summary
// and a verbatim tail, the summary becomes the session's resume point, and
// /resume rebuilds summary + tail without showing the tail twice. The fresh
// turn flow no longer compacts before a send (nothing carries between
// turns), so the test drives the machinery the mid-turn path runs.
func TestCompaction(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	reflectStub(t, h, "ok")
	h.reflectMu.Lock() // no detached reflect racing the DB teardown
	defer h.reflectMu.Unlock()
	send := sender(t, h)

	send(strings.Repeat("x", 80_000)) // 20k tokens: past the 15k tail budget
	first := h.AgentSessionID()
	h.agents.mu.Lock()
	h.agents.ctxWin = 100_000
	h.agents.mu.Unlock()
	if err := h.DB.SetTodos(first, []store.Todo{{Content: "ship </summary>", Status: "pending"}}); err != nil {
		t.Fatal(err)
	}
	if !h.compactReporting(t.Context(), h.agents.runner, compactAuto, "") {
		t.Fatal("the compaction failed")
	}
	summary := h.agents.runner.History[0]
	if prev, _ := summaryOf(summary); !strings.Contains(concatText(summary.Content), "- [pending] ship") || strings.Contains(prev, "ship") {
		t.Fatalf("the todo list must follow the summary and stay out of the next merge: %q", concatText(summary.Content))
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
	if oks != 1 {
		t.Fatalf("resumed transcript shows %d replies, want the tail's 1", oks)
	}
	// the resume sends the stored context, and stores no other
	rows, _ := h.DB.Messages(first)
	if n := len(slices.DeleteFunc(rows, func(r *store.Message) bool { return r.Role != contextRole })); n != 1 {
		t.Fatalf("the session stored %d context messages, want 1", n)
	}
}

// TestCompactionOnCachedPrefix: on the conversation's own model the summary
// is asked after the runner's system prompt and whole history, the prefix the
// last step read, with a note naming the verbatim tail; a refusal there falls
// back to the flat transcript, which
// still summarizes instead of dropping the history.
func TestCompactionOnCachedPrefix(t *testing.T) {
	for _, name := range []string{"answers", "refused"} {
		t.Run(name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			h.Settings.SetQuiet("models.default", "stub/m")
			m := reflectStub(t, h, "ok")
			h.reflectMu.Lock()
			defer h.reflectMu.Unlock()
			send := sender(t, h)
			big := strings.Repeat("x", 80_000) // past the tail budget, so it is the head
			send(big)
			h.agents.mu.Lock()
			h.agents.ctxWin = 100_000
			sys := h.agents.pin.text
			h.agents.mu.Unlock()
			if name == "refused" {
				m.overflow.Store(1)
			}
			if !h.compactReporting(t.Context(), h.agents.runner, compactAuto, "") {
				t.Fatal("the compaction failed")
			}
			m.mu.Lock()
			p := m.prompts[1]
			m.mu.Unlock()
			if len(p) != 5 || concatText(p[0].Content) != sys || concatText(p[2].Content) != big || concatText(p[3].Content) != "ok" ||
				!strings.HasPrefix(concatText(p[4].Content), compactNote) || !strings.Contains(concatText(p[4].Content), "except its last 1 message,") {
				t.Fatalf("the compaction call is not system, context, whole history, note naming the tail: %d messages", len(p))
			}
			if s, _ := summaryOf(h.agents.runner.History[0]); s != "ok" {
				t.Fatalf("summary %q", s)
			}
		})
	}
}

// TestHandoffGate: a send whose pinned prefix changed since the last step
// offers a compact on the previous model once, when the context is big
// enough and the provider reported cache; agent.handoff pins the config.
// The fresh-turn flow no longer calls the gate (nothing carries between
// turns), so the test forges the state a send would have left and calls it.
func TestHandoffGate(t *testing.T) {
	for _, c := range []struct {
		mode    string
		wantAsk bool
	}{
		{"", true},
		{"off", false},
		{"on", false}, // compacts silently
		{"ask", true},
	} {
		t.Run(cmp.Or(c.mode, "default"), func(t *testing.T) {
			h, _ := newTestHostDB(t)
			h.Settings.SetQuiet("models.default", "stub/m")
			reflectStub(t, h, "ok")
			h.reflectMu.Lock()
			defer h.reflectMu.Unlock()
			ui := &fakeUI{qOK: true, qAnswer: "Send as is"}
			h.SetUIHooks(ui)
			send := sender(t, h)
			send(strings.Repeat("x", 80_000)) // the step that lastSent names
			h.agents.mu.Lock()
			h.agents.lastIn, h.agents.lastCache = 90_000, 1 // the gate's floor is 50k
			h.agents.mu.Unlock()
			h.Settings.SetQuiet("models.default", "stub/m2")
			if c.mode != "" {
				h.Settings.SetQuiet(config.KeyAgentHandoff, c.mode)
			}
			h.agents.mu.Lock()
			h.agents.turnModel = "stub/m2" // what the send's runner would run
			h.agents.mu.Unlock()
			h.pinPrompt(h.mainTools()) // what a send would build before its gate
			h.handoffGate(nil, nil, "stub/m", &turnScope{ctx: context.Background()})
			if got := len(ui.qAsked) > 0; got != c.wantAsk {
				t.Fatalf("asked = %v, want %v (%+v)", got, c.wantAsk, ui.qAsked)
			}
			if !c.wantAsk {
				return
			}
			if q := ui.qAsked[0].Question; !strings.Contains(q, "stub/m") || !strings.Contains(q, "stub/m2") {
				t.Fatalf("the ask names the models: %q", q)
			}
			// the same change never asks again
			h.handoffGate(nil, nil, "stub/m", &turnScope{ctx: context.Background()})
			if len(ui.qAsked) != 1 {
				t.Fatalf("asked %d times for one change", len(ui.qAsked))
			}
		})
	}
}

// TestCompactionMidTurn: a step that fills the context cuts the turn before
// the model reads its tool result; the history is compacted and the same
// turn continues from the summary.
func TestCompactionMidTurn(t *testing.T) {
	for _, c := range []struct {
		name       string
		summaryErr error
		cancel     bool // esc while the summary runs
		streams    int32
		errRow     string // the error row's status, "" for no error row
	}{
		{"continues after the summary", nil, false, 2, ""},
		{"ends the turn when the summary fails", errors.New("summary down"), false, 1, ""},
		{"esc during the summary reads as interrupted", nil, true, 1, "cancelled"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestHostDB(t)
			m := reflectStub(t, h, "ok")
			m.err = c.summaryErr
			h.reflectMu.Lock()
			defer h.reflectMu.Unlock()
			h.agents.mu.Lock()
			h.agents.ctxWin = 100_000
			h.agents.mu.Unlock()
			m.toolStep.Store(90_000)
			ui := &fakeUI{}
			h.SetUIHooks(ui)
			// the summary runs inside the turn: the working rule has a clock
			// to show "Compacting" against, and esc reaches it
			// a title call runs its own Generate beside the turn's, so both
			// fields it and the model share move atomically
			var elapsed atomic.Int64
			m.during = func() {
				// the title's call comes before the turn's first stream, so
				// only the summary's call, inside the compaction, acts here
				if s := h.AgentStream(); s.Compacting {
					elapsed.Store(int64(s.Elapsed))
					if c.cancel {
						h.AgentCancel()
						m.block.Store(int64(time.Second))
					}
				}
			}
			sender(t, h)("go")
			if elapsed.Load() <= 0 {
				t.Fatal("no turn clock while summarizing")
			}
			if n := m.streams.Load(); n != c.streams {
				t.Fatalf("model streamed %d times, want %d", n, c.streams)
			}
			// the cut ends a send, not the turn: one finish for the user,
			// also when the compaction it waits for fails
			if n := ui.turnsDone.Load(); n != 1 {
				t.Fatalf("TurnDone ran %d times, want 1", n)
			}
			if c.summaryErr == nil {
				if n := m.lastPrompt.Load(); n != 3 {
					t.Fatalf("continued prompt has %d messages, want 3 (system, context, summary)", n)
				}
			}
			var kinds []string
			errRow := ""
			for _, row := range h.Session().Transcript {
				kinds = append(kinds, string(row.Kind))
				if row.Kind == "error" {
					errRow = string(row.Status)
				}
			}
			if got := strings.Join(kinds, ","); strings.Contains(got, "compact") != (c.summaryErr == nil && !c.cancel) || errRow != c.errRow {
				t.Fatalf("transcript kinds: %s, error row %q", got, errRow)
			}
			if u := h.agents.cutUsage; u != (fantasy.Usage{}) {
				t.Fatalf("cut usage still banked: %+v", u)
			}
		})
	}
}

// TestFailedSummaryRowSaysDropped: an auto compaction whose summary call
// fails drops the older history unsummarized, and its row says so; the
// resumed transcript, which rebuilds the row from the stored summary, shows
// what the live turn drew.
func TestFailedSummaryRowSaysDropped(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	reflectStub(t, h, "") // an empty reply summarizes to nothing: the history drops
	h.reflectMu.Lock()
	defer h.reflectMu.Unlock()
	send := sender(t, h)
	send("warm up")
	h.agents.mu.Lock()
	h.agents.ctxWin = 100_000
	h.agents.mu.Unlock()
	h.compactReporting(t.Context(), h.agents.runner, compactAuto, "")
	row := ""
	for _, r := range h.Session().Transcript {
		if r.Kind == RowCompact {
			row = r.Text
		}
	}
	if !strings.Contains(row, "the summary failed") || !strings.Contains(row, "were dropped unsummarized") {
		t.Fatalf("compaction row %q, want the failed summary and the dropped count", row)
	}

	first := h.AgentSessionID()
	if err := h.NewSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.ResumeSession(first); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range h.Session().Transcript {
		if r.Kind == RowCompact {
			n++
			if r.Text != row {
				t.Fatalf("resumed compaction row %q, want what the live turn drew (%q)", r.Text, row)
			}
		}
	}
	if n != 1 {
		t.Fatalf("resumed transcript has %d compaction rows, want 1", n)
	}
}

// TestOverflowRecovery: a prompt the provider refuses as too long is
// compacted and resent once without a second copy of the user message; a
// second refusal is reported, not retried. An earlier turn is summarized
// while the refused request stays verbatim.
func TestOverflowRecovery(t *testing.T) {
	for _, c := range []struct {
		name     string
		earlier  bool
		refusals int32
		prompt   int32
		wantErr  bool
	}{
		{"recovers", false, 1, 3, false},
		{"reports the second refusal", false, 2, 3, true},
		{"keeps the request verbatim", true, 1, 4, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestHostDB(t)
			m := reflectStub(t, h, "ok")
			h.reflectMu.Lock()
			defer h.reflectMu.Unlock()
			send := sender(t, h)
			if c.earlier {
				send("earlier")
			}
			before := m.streams.Load()
			m.overflow.Store(c.refusals)
			send("hi")
			if n := m.streams.Load() - before; n != 2 {
				t.Fatalf("model streamed %d times, want 2", n)
			}
			if n := m.lastPrompt.Load(); n != c.prompt {
				t.Fatalf("resent prompt has %d messages, want %d (system, context, summary, then the request when a turn came before it)", n, c.prompt)
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
	t.Parallel()
	h, _ := newTestHostDB(t)
	if _, err := h.Settings.SetQuiet("session.auto_title", false); err != nil {
		t.Fatal(err)
	}
	m := reflectStub(t, h, "") // an empty reply starts no title call
	m.err = errors.New("summarizer down")
	h.reflectMu.Lock()
	defer h.reflectMu.Unlock()
	send := sender(t, h)
	send("warm up")
	for range compactMaxFails {
		h.agents.mu.Lock()
		h.agents.ctxWin = 100_000
		h.agents.mu.Unlock()
		h.compactReporting(t.Context(), h.agents.runner, compactAuto, "")
	}
	if n := m.calls.Load(); n != compactMaxFails {
		t.Fatalf("summarizer called %d times, want %d", n, compactMaxFails)
	}
	// the failure that turns it off is one toast, not its own and another
	h.evlog.mu.Lock()
	defer h.evlog.mu.Unlock()
	var titles []string
	for _, n := range h.evlog.notes {
		titles = append(titles, n.title)
	}
	if want := []string{"Could not compact", "Could not compact", "Auto compaction off"}; !slices.Equal(titles, want) {
		t.Fatalf("toasts %q, want %q", titles, want)
	}
}

func TestTailStart(t *testing.T) {
	user := func(bytes int) fantasy.Message { return agent.UserMessage(strings.Repeat("u", bytes)) }
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

// TestCompactThreshold: the trigger is agent.compact_at percent of the
// window, never under half of it, and nothing on an unknown window.
func TestCompactThreshold(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	for _, c := range []struct {
		window, at, want int64
	}{
		{0, 80, 0},
		{200_000, 80, 160_000},
		{128_000, 50, 64_000},
		{32_000, 20, 16_000}, // never under half the window
		{200_000, 100, 200_000},
	} {
		if _, err := h.Settings.SetQuiet("agent.compact_at", float64(c.at)); err != nil {
			t.Fatal(err)
		}
		if got := h.compactThreshold(c.window); got != c.want {
			t.Errorf("compactThreshold(%d, %d) = %d, want %d", c.window, c.at, got, c.want)
		}
	}
}

func TestPruneToolOutput(t *testing.T) {
	for _, c := range []struct {
		name      string
		threshold int64
		kept      int
		oldTokens int
		wantFreed int64
	}{
		{"a small saving clears nothing", 0, toolKeepTokens, 10_000, 0},
		{"old output past the kept 40k is cleared", 0, toolKeepTokens, 30_000, 30_000},
		{"a 160k threshold keeps 40k too", 160_000, toolKeepTokens, 10_000, 0},
		{"a 16k threshold keeps 4k", 16_000, 4_000, 10_000, 10_000},
		{"a small threshold's saving has a floor", 16_000, 4_000, 3_000, 0},
	} {
		history := []fantasy.Message{toolResult("old", c.oldTokens), toolResult("new", c.kept)}
		out, freed := pruneToolOutput(history, c.threshold, false)
		if freed != c.wantFreed {
			t.Errorf("%s: freed %d, want %d", c.name, freed, c.wantFreed)
		}
		cleared := toolText(out[0].Content[0].(fantasy.ToolResultPart)) == clearedOutput
		if cleared != (c.wantFreed > 0) || estTokens(out[1]) != int64(c.kept) || estTokens(history[0]) != int64(c.oldTokens) {
			t.Errorf("%s: cleared %v, newest %d tokens, input %d tokens", c.name, cleared, estTokens(out[1]), estTokens(history[0]))
		}
	}
}

// TestPruneKeepsUserWords: a question result is the only place the user's
// answer lives in the model's history, so pruning must leave it.
func TestPruneKeepsUserWords(t *testing.T) {
	history := []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "old", ToolName: "question"},
		}},
		toolResult("old", 30_000), toolResult("new", toolKeepTokens),
	}
	if _, freed := pruneToolOutput(history, 0, true); freed != 0 {
		t.Fatalf("the user's answer was cleared: %d tokens", freed)
	}
}

// TestPruneKeepsRunningTurn: pruning between the steps of one turn must
// leave the output after the last user message, which the turn continues
// from, even when it is past the kept budget.
func TestPruneKeepsRunningTurn(t *testing.T) {
	history := []fantasy.Message{
		agent.UserMessage("old ask"), toolResult("old", 30_000),
		agent.UserMessage("live ask"), toolResult("live", 50_000),
	}
	out, freed := pruneToolOutput(history, 0, false)
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
	history := []fantasy.Message{agent.UserMessage("look"), toolResult("old", 30_000), toolResult("new", toolKeepTokens)}
	if _, freed := pruneToolOutput(history, 0, false); freed != 0 {
		t.Fatalf("a running turn's output was cleared: %d tokens", freed)
	}
	out, freed := pruneToolOutput(history, 0, true)
	if freed != 30_000 || toolText(out[1].Content[0].(fantasy.ToolResultPart)) != clearedOutput {
		t.Fatalf("freed %d tokens, old output %q", freed, toolText(out[1].Content[0].(fantasy.ToolResultPart)))
	}
	if estTokens(out[2]) != toolKeepTokens {
		t.Fatalf("the newest output was cleared too: %d tokens", estTokens(out[2]))
	}
}

// TestTranscriptTextNotesAttachments: a compacted transcript names attached
// files, which the summarizer cannot see, and sets what zemi appended apart
// from what the user typed.
func TestTranscriptTextNotesAttachments(t *testing.T) {
	got := transcriptText([]fantasy.Message{{
		Role: fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "what is wrong here" + agent.Note("mark it done")},
			fantasy.FilePart{MediaType: "image/png", Data: []byte("x")},
		},
	}})
	if !strings.Contains(got, "user: what is wrong here\nattached, not typed by the user: <zemi-note>\nmark it done\n</zemi-note>\n") || !strings.Contains(got, "[attached image/png]") {
		t.Fatalf("transcript = %q", got)
	}
	// a joined queue carries several blocks: none of their closes leak
	joined := "what is wrong here" + agent.Attached("loop", "Run 1 of: check") + agent.Attached("zemi-note", "mark it")
	got = transcriptText([]fantasy.Message{{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: joined}}}})
	if strings.Contains(got, "</attached-files>") {
		t.Fatalf("a joined queue's closes leak: %q", got)
	}
}

func toolResult(id string, tokens int) fantasy.Message {
	return fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
		fantasy.ToolResultPart{ToolCallID: id, Output: fantasy.ToolResultOutputContentText{Text: strings.Repeat("r", tokens*4)}},
	}}
}

// TestCapRefusal: a refused output cap is dropped and resent without an
// error row, and Anthropic's context overflow, which names max_tokens too,
// is compacted instead, off the refused prefix, and keeps the cap.
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
			runUser(t, r, `zemi.provider.add({ id = "a", kind = "anthropic", models = { { id = "m", max_output = 8000 } } })`)
			t.Cleanup(func() { capRejected.Delete("a/m") })
			m := reflectStub(t, h, "ok")
			m.failOnce.Store(&fantasy.ProviderError{StatusCode: 400, Message: c.msg})
			h.reflectMu.Lock()
			defer h.reflectMu.Unlock()
			sender(t, h)("hi")
			if n := m.streams.Load(); n != 2 {
				t.Fatalf("model streamed %d times, want 2 (the refused request and its resend)", n)
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
