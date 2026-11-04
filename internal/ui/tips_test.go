package ui

import (
	"testing"
	"time"
)

// TestTip: the exit tip is stable within an hour and cycles over every tip.
func TestTip(t *testing.T) {
	t.Parallel()
	hour := time.Unix(1767225600, 0)
	if got := Tip(hour); got != Tip(hour.Add(59*time.Minute)) {
		t.Fatalf("the tip changed within the hour: %q vs %q", Tip(hour), Tip(hour.Add(59*time.Minute)))
	}
	seen := map[string]bool{}
	for i := range _tips {
		seen[Tip(hour.Add(time.Duration(i)*time.Hour))] = true
	}
	if len(seen) != len(_tips) {
		t.Fatalf("%d tips, %d shown over %d hours", len(_tips), len(seen), len(_tips))
	}
}
