package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

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

// TestWindowClickHitsTopmost: a click where a float covers a split goes to
// the float, which is drawn over it.
func TestWindowClickHitsTopmost(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.CaptureHooks() // window exits post from their own goroutines
	defer a.host.Wins.CloseAll()
	var ids []int
	for _, kind := range []string{win.KindSplit, win.KindFloat} {
		w, err := a.host.Wins.Open(win.Spec{Cmd: "sleep 5", Kind: kind})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, w.ID)
	}
	a.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	a.Frame()
	rect := func(id int) winRect {
		for _, r := range a.winRects {
			if r.id == id {
				return r
			}
		}
		t.Fatalf("window %d not drawn", id)
		return winRect{}
	}
	split, float := rect(ids[0]), rect(ids[1])
	x, y := float.x, split.y
	if x >= split.x+split.w || y < float.y || y >= float.y+float.h {
		t.Fatalf("float %+v does not cover split %+v", float, split)
	}
	a.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	if a.focusWin != ids[1] {
		t.Fatalf("click focused window %d, want the float %d", a.focusWin, ids[1])
	}
}

// TestWinTitleRowFits: the title row never outgrows its window, which would
// shift every column joined after it, and keeps the close mark.
func TestWinTitleRowFits(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	defer a.host.Wins.CloseAll()
	wn, err := a.host.Wins.Open(win.Spec{Cmd: "sleep 5", Kind: win.KindVSplit})
	if err != nil {
		t.Fatal(err)
	}
	for _, width := range []int{2, 10, 20, 80} {
		row := a.winTitleRow(wn, width, true)
		if got := plainSegs(row); segsWidth(row) != width || !strings.HasSuffix(got, "× ") {
			t.Fatalf("width %d: %q is %d cells", width, got, segsWidth(row))
		}
	}
}

func TestWidgetWidthIsMainColumn(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	defer a.host.Wins.CloseAll()
	a.width = 120
	if _, err := a.host.Wins.Open(win.Spec{Cmd: "sleep 5", Kind: win.KindVSplit}); err != nil {
		t.Fatal(err)
	}
	if _, _, mainW := a.winColumns(a.width); a.widgetWidth() != mainW || mainW >= 120 {
		t.Fatalf("widget width %d, main column %d", a.widgetWidth(), mainW)
	}
}
