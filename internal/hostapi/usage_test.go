package hostapi

import (
	"strings"
	"testing"
)

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
