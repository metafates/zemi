package hostapi

import (
	"context"
	"fmt"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/store"
)

// TestTurnCost: prices registered from Lua price a turn, cache writes and
// reads at their own prices; a model without prices costs nothing.
func TestTurnCost(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	runUser(t, r, `zemi.provider.add({
		id = "p", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1",
		models = {
			{ id = "m", cost_per_1m_in = 3, cost_per_1m_out = 15,
			  cost_per_1m_cache_write = 3.75, cost_per_1m_cache_read = 0.5 },
			{ id = "free" },
		},
	})`)
	u := fantasy.Usage{
		InputTokens: 2_000_000, OutputTokens: 100_000,
		CacheCreationTokens: 1_000_000, CacheReadTokens: 10_000_000,
	}
	if got := h.turnCost("p/m", u); got != 16.25 {
		t.Fatalf("priced turn = %v, want 16.25", got)
	}
	if got := h.turnCost("p/free", u); got != 0 {
		t.Fatalf("unpriced model = %v, want 0", got)
	}
}

// TestAgentFinishUsage: agent.finish carries the turn's own tokens and cost,
// and zemi.agent.usage() the session's, cost included.
func TestAgentFinishUsage(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	sid := seedSession(t, h)
	runUser(t, r, `
		zemi.provider.add({ id = "p", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1",
			models = { { id = "m", cost_per_1m_in = 1, cost_per_1m_out = 2 } } })
		zemi.config.set("models.default", "p/m")
		zemi.event.on("agent.finish", function(ev)
			local u, s = ev.usage, zemi.agent.usage()
			zemi.state.set("fin", table.concat({ ev.session, ev.model, u["in"], u.out, u.total, u.cost, s.cost, s.total, s.context.window }, " "))
		end)`)
	for range 2 {
		u := fantasy.Usage{InputTokens: 1_000_000, CacheReadTokens: 5, OutputTokens: 500_000}
		h.streamHooks().StreamDone(u)
		h.streamHooks().TurnDone(nil, u)
	}
	want := `"` + sid + ` p/m 1000005 500000 1500005 2 4 3000010 0"`
	waitFor(t, func() error {
		if v, _ := h.GlobalDB.GetKV("fin"); v != want {
			return fmt.Errorf("agent.finish = %s, want %s", v, want)
		}
		return nil
	})
}

// TestUsageCountsCache: the conversation's steps bill the "main" anchor row
// and a subagent's its parent session, cache reads and writes included in
// both the live tally and the stored rows.
func TestUsageCountsCache(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	run, _ := h.newRun(t.Context(), builtinAgents[0], "d", "p", false)
	u := fantasy.Usage{InputTokens: 1, CacheReadTokens: 10, CacheCreationTokens: 100, OutputTokens: 5}
	h.streamHooks().StreamDone(u)
	(&subagentHooks{h: h, def: builtinAgents[0], run: run}).TurnDone(nil, u)
	if got := h.AgentStream().SessionIn; got != 222 {
		t.Errorf("live input tally = %d, want 222", got)
	}
	// the run bills its parent, the "main" anchor row the conversation also
	// rides: one row now holds both
	if sess, err := h.DB.GetSession(store.MainSessionID); err != nil || sess.PromptTokens != 222 {
		t.Errorf("anchor row usage = %+v, %v; want 222", sess, err)
	}
	// the dated buckets read the anchor's accounting: the /usage windows
	// cannot read zero against the session the screen shows
	u2, err := h.DB.UsageSince(0)
	if err != nil || u2.Prompt != 222 || u2.Completion != 10 {
		t.Errorf("all-time buckets = %+v, %v; want the anchor's 222 in, 10 out", u2, err)
	}
}

// TestStreamBillsLiveTally: a model call's usage lands in the session tally
// and on a subagent run's row as its stream finishes, before its tools run,
// so a count moves during the turn instead of only when it ends. The
// conversation's own call bills to the "main" anchor row.
func TestStreamBillsLiveTally(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	seedSession(t, h)
	u := fantasy.Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 3}
	h.streamHooks().StreamDone(u)
	run, _ := h.newRun(t.Context(), builtinAgents[0], "d", "p", false)
	(&subagentHooks{h: h, def: builtinAgents[0], run: run}).StreamDone(u)
	if tok, _ := run.usage(); tok != 18 {
		t.Fatalf("run tokens after one call = %d, want 18", tok)
	}
	if got := h.AgentStream().SessionIn; got != 13 {
		t.Fatalf("live input tally after one step = %d, want 13", got)
	}
	if got := h.AgentStream().SessionOut; got != 5 {
		t.Fatalf("live output tally after one step = %d, want 5", got)
	}
	if sess, err := h.DB.GetSession(store.MainSessionID); err != nil || sess.PromptTokens != 13 {
		t.Fatalf("anchor row usage = %+v, %v; want 13", sess, err)
	}
}

// streamOnly answers every call with a stream; its Generate fails.
type streamOnly struct{ *scriptModel }

func (streamOnly) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return streamOf(&fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "a title"}}, FinishReason: fantasy.FinishReasonStop}), nil
}

// TestGenerateStreams pins generate's streaming: Anthropic's SDK refuses a
// large max_tokens without it.
func TestGenerateStreams(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	h.modelFn = func() (fantasy.LanguageModel, error) { return streamOnly{}, nil }
	maxOut := int64(128_000)
	res, err := h.generate(t.Context(), store.MainSessionID, "Name the conversation:", "hi", &maxOut, nil, "compact")
	if err != nil || res.Response.Content.Text() != "a title" {
		t.Fatalf("generate = %+v, %v", res, err)
	}
}
