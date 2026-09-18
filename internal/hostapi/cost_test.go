package hostapi

import (
	"context"
	"testing"

	"charm.land/fantasy"
)

// TestTurnCost: prices registered from Lua price a turn, cache writes and
// reads at their own prices; a model without prices costs nothing.
func TestTurnCost(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `zemi.provider.add({
		id = "p", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1",
		models = {
			{ id = "m", cost_per_1m_in = 3, cost_per_1m_out = 15,
			  cost_per_1m_in_cached = 3.75, cost_per_1m_out_cached = 0.5 },
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

// TestUsageCountsCache: main and subagent steps add cache reads and writes to
// the input tally, live and stored alike, so a resumed session shows the
// same total.
func TestUsageCountsCache(t *testing.T) {
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)
	run, _ := h.newRun(context.Background(), builtinAgents[0], "d", "p", false)
	u := fantasy.Usage{InputTokens: 1, CacheReadTokens: 10, CacheCreationTokens: 100, OutputTokens: 5}
	h.streamHooks().TurnDone(nil, u)
	(&subagentHooks{h: h, def: builtinAgents[0], run: run}).TurnDone(nil, u)
	if got := h.AgentStream().SessionIn; got != 222 {
		t.Errorf("live input tally = %d, want 222", got)
	}
	if sess, err := h.DB.GetSession(sid); err != nil || sess.PromptTokens != 222 {
		t.Errorf("stored input tokens = %+v, %v; want 222", sess, err)
	}
	if err := h.NewSession(); err != nil {
		t.Fatal(err)
	}
	if s := h.AgentStream(); s.CacheRead != 0 || s.CacheBilled != 0 {
		t.Errorf("cache share after /new = %d/%d, want 0/0", s.CacheRead, s.CacheBilled)
	}
}
