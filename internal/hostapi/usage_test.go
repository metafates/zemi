package hostapi

import (
	"log/slog"
	"strings"
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
		t.Cleanup(h.Close)
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

// TestUsageView: /usage names the view's own size beside the token tallies -
// the bytes it fills, its lines and levels, and the lines still unbuilt.
func TestUsageView(t *testing.T) {
	t.Parallel()
	h, ui := seedMemories(t)
	if err := h.OpenUsage(); err != nil {
		t.Fatal(err)
	}
	o := ui.opened[len(ui.opened)-1]
	if o.ID != "usage" {
		t.Fatalf("opened %q", o.ID)
	}
	want := []string{"View", "4 lines", "2 levels", "1 line unbuilt"}
	for _, s := range want {
		if !strings.Contains(o.Body, s) {
			t.Errorf("usage reads no %q:\n%s", s, o.Body)
		}
	}
	// an empty view reads no View section
	h2, _ := newTestHostDB(t)
	ui2 := &fakeUI{}
	h2.SetUIHooks(ui2)
	if err := h2.OpenUsage(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ui2.opened[len(ui2.opened)-1].Body, "View") {
		t.Error("an empty view drew a View section")
	}
}
