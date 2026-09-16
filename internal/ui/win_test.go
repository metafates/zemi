package ui

import (
	"testing"

	"github.com/metafates/zemi/internal/win"
)

// TestShares: fixed shares hold, the rest split what is left with the
// main area, and the main area keeps its minimum.
func TestShares(t *testing.T) {
	m := &win.Manager{}
	var wins []*win.Window
	for _, f := range []float64{0, 0.5, 0} {
		w, err := m.Open(win.Spec{Cmd: "sleep 5", Kind: win.KindVSplit})
		if err != nil {
			t.Fatal(err)
		}
		_ = w.SetLayout(func(l *win.Layout) { l.Frac = f })
		wins = append(wins, w)
	}
	defer m.CloseAll()
	got := shares(wins, 200, 20, 30)
	if got[1] != 100 || got[0] != 23 || got[2] != 23 {
		t.Fatalf("shares %v", got)
	}
	if got := shares(wins, 50, 20, 30); got[0]+got[1]+got[2] > 60 {
		t.Fatalf("tight frame: %v", got)
	}
	if shares(nil, 100, 20, 30) != nil {
		t.Fatal("no windows, no shares")
	}
}
