package ui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

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
	// the float's title sits in its top border, right above its cells
	if top := plainSegs(a.lastRows[float.titleY]); float.titleY != float.y-1 || !strings.Contains(top, "╭─ 2 sleep 5 ─") || !strings.Contains(top, "× ─╮") {
		t.Fatalf("float top border %q at %d, cells at %d", top, float.titleY, float.y)
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

func TestKittyHostDetect(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.Update(tea.ColorProfileMsg{Profile: colorprofile.ANSI256})
	for name, want := range map[string]bool{"ghostty 1.3.1": true, "kitty(0.42.1)": true, "WezTerm 20240203": false, "tmux 3.5a": false} {
		a.Update(tea.TerminalVersionMsg{Name: name})
		if a.host.Wins.Kitty() != want {
			t.Fatalf("%s: kitty %v", name, !want)
		}
	}
	a.Update(tea.TerminalVersionMsg{Name: "ghostty 1.3.1"})
	if a.Update(tea.ColorProfileMsg{Profile: colorprofile.ASCII}); a.host.Wins.Kitty() {
		t.Fatal("NO_COLOR drops the placeholder colors, yet images stay on")
	}
}

// TestImageCellsFollowTheFloat: an image is placeholder cells, so it moves
// with its float, a float on top covers it, hiding drops it, and closing
// deletes it from the host.
func TestImageCellsFollowTheFloat(t *testing.T) {
	const ph = "\U0010EEEE"
	a := newTestApp(t, t.TempDir())
	a.CaptureHooks()
	defer a.host.Wins.CloseAll()
	a.host.Wins.SetKitty(true)
	wn, err := a.host.Wins.Open(win.Spec{Cmd: `printf '\033_Ga=T,f=100,c=3,r=2,q=2;iVBO\033\\'; sleep 5`, Kind: win.KindFloat, Cols: 20, Rows: 6})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for !strings.Contains(fmt.Sprint(wn.Screen().Rows[0]), ph) {
		if ctx.Err() != nil {
			t.Fatal("the image never reached the screen")
		}
		time.Sleep(20 * time.Millisecond)
	}
	a.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	// imageAt is the column of the image on the float's first terminal row,
	// -1 when the row shows none
	imageAt := func() int {
		frame := strings.Split(a.Frame(), "\r\n")
		for _, r := range a.winRects {
			if r.id != wn.ID {
				continue
			}
			row := ansi.Strip(frame[r.y])
			if before, _, ok := strings.Cut(row, ph); ok {
				return ansi.StringWidth(before)
			}
		}
		return -1
	}
	move := func(wn *win.Window, x int) {
		_ = wn.SetLayout(func(l *win.Layout) { l.X, l.Y = x, 4 })
	}
	move(wn, 10)
	if got := imageAt(); got != 12 || !strings.Contains(a.Frame(), "38;5;16m"+ph) {
		t.Fatalf("image at column %d, want 12 in the host's id color", got)
	}
	move(wn, 15)
	if got := imageAt(); got != 17 {
		t.Fatalf("moved image at column %d, want 17", got)
	}
	top, err := a.host.Wins.Open(win.Spec{Cmd: "sleep 5", Kind: win.KindFloat, Cols: 20, Rows: 6})
	if err != nil {
		t.Fatal(err)
	}
	move(top, 15)
	if strings.Contains(a.Frame(), ph) {
		t.Fatal("the float on top did not cover the image")
	}
	a.host.Wins.Close(top.ID)
	_ = wn.SetLayout(func(l *win.Layout) { l.Hidden = true })
	if strings.Contains(a.Frame(), ph) {
		t.Fatal("a hidden window still shows its image")
	}
	_ = wn.SetLayout(func(l *win.Layout) { l.Hidden = false })
	if got := imageAt(); got != 17 {
		t.Fatalf("shown again, image at column %d", got)
	}
	if g := a.host.Wins.TakeGraphics(); !strings.Contains(g, "a=p,U=1,i=16") {
		t.Fatalf("host got %q", g)
	}
	a.host.Wins.Close(wn.ID)
	if g := a.host.Wins.TakeGraphics(); !strings.Contains(g, "a=d,d=I,i=16") {
		t.Fatalf("close queued %q", g)
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
