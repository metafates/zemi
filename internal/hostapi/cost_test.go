package hostapi

import (
	"context"
	"fmt"
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

// TestAgentFinishUsage: agent.finish carries the turn's own tokens and cost,
// and zemi.agent.usage() the session's, cost included.
func TestAgentFinishUsage(t *testing.T) {
	h, r := newTestHostDB(t)
	sid := seedSession(t, h)
	runUser(t, r, `
		zemi.provider.add({ id = "p", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1",
			models = { { id = "m", cost_per_1m_in = 1, cost_per_1m_out = 2 } } })
		zemi.config.set("models.main", "p/m")
		zemi.event.on("agent.finish", function(ev)
			local u, s = ev.usage, zemi.agent.usage()
			zemi.state.set("fin", table.concat({ ev.session, ev.model, u["in"], u.out, u.total, u.cost, s.cost, s.total, s.context_window }, " "))
		end)`)
	for range 2 {
		h.streamHooks().TurnDone(nil, fantasy.Usage{InputTokens: 1_000_000, CacheReadTokens: 5, OutputTokens: 500_000})
	}
	want := `"` + sid + ` p/m 1000005 500000 1500005 2 4 3000010 0"`
	waitFor(t, func() error {
		if v, _ := h.GlobalDB.GetKV("fin"); v != want {
			return fmt.Errorf("agent.finish = %s, want %s", v, want)
		}
		return nil
	})
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
