package hostapi

import (
	"log/slog"
	"testing"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
)

// TestUsagePriced: the session tally says whether the conversation's model
// declares prices - what the cost widget shows - however the tally gathered
// its cost.
func TestUsagePriced(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	for _, c := range []struct {
		prices [2]float64 // CostPer1MIn, CostPer1MOut
		priced bool
	}{
		{[2]float64{0, 0}, false},
		{[2]float64{1.5, 0}, true},
		{[2]float64{0, 7}, true},
	} {
		r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
		h := NewHost(r, log, config.NewStore(), config.Dirs{})
		if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
			t.Fatal(err)
		}
		// the boot seeds the registry with the builtins; the spec goes after
		h.Providers.Reset(agent.Catalog{Specs: []agent.Spec{{ID: "p", Models: []agent.Model{{
			ID: "m", CostPer1MIn: c.prices[0], CostPer1MOut: c.prices[1],
		}}}}, Keys: map[string]string{"p": "x"}})
		h.SetModel("p/m")
		if got := h.modelPriced(h.turnModelID()); got != c.priced {
			t.Errorf("prices %v: priced %v, want %v", c.prices, got, c.priced)
		}
	}
}
