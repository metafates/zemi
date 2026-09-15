package hostapi

import (
	"testing"

	"github.com/metafates/zemi/internal/agent"
)

func TestTurnCost(t *testing.T) {
	h, _ := newTestHost(t)
	if err := h.Providers.Register(agent.Spec{ID: "p", Kind: "openai_compat", BaseURL: "http://127.0.0.1:1/v1",
		Models: []agent.Model{{ID: "m", CostPer1MIn: 3, CostPer1MOut: 15}}}); err != nil {
		t.Fatal(err)
	}
	if got := h.turnCost("p/m", 2_000_000, 100_000); got != 7.5 {
		t.Fatalf("priced turn = %v, want 7.5", got)
	}
	if got := h.turnCost("p/unpriced", 1000, 1000); got != 0 {
		t.Fatalf("unknown model = %v, want 0", got)
	}
}
