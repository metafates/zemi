package hostapi

import (
	"fmt"
	"testing"

	"charm.land/fantasy"
)

// TestAgentFinishUsage: agent.finish carries the turn's own tokens and cost,
// and zemi.agent.usage() the session's, cost included.
func TestAgentFinishUsage(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	sid := seedSession(t, h)
	runUser(t, r, `
		zemi.provider.add({ id = "p", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1",
			models = { { id = "m", pricing = { input = 1, output = 2 } } } })
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
