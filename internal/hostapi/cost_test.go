package hostapi

import "testing"

// TestTurnCost: prices registered from Lua price a turn; a model without
// prices costs nothing.
func TestTurnCost(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `zemi.provider.register({
		id = "p", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1",
		models = { { id = "m", cost_per_1m_in = 3, cost_per_1m_out = 15 }, { id = "free" } },
	})`)
	if got := h.turnCost("p/m", 2_000_000, 100_000); got != 7.5 {
		t.Fatalf("priced turn = %v, want 7.5", got)
	}
	if got := h.turnCost("p/free", 1000, 1000); got != 0 {
		t.Fatalf("unpriced model = %v, want 0", got)
	}
}
