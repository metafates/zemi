package ui

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/win"
)

// TestVsplitShares: a vsplit's frac is of the row's cells after the
// separators, the others share what is left with the main column, cells
// above their mins split equally, and on a narrow screen the windows give
// way before the main column goes below its min.
func TestVsplitShares(t *testing.T) {
	for _, c := range []struct {
		name     string
		width    int
		fracs    []float64
		wantMain int
		wantWins []int
	}{
		{"frac and equal shares", 200, []float64{0, 0.5, 0}, 40, []int{29, 99, 29}},
		{"narrow: windows shrink first", 60, []float64{0, 0.5, 0}, 30, []int{7, 20}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newTestApp(t, t.TempDir())
			defer a.host.Wins.CloseAll()
			for _, f := range c.fracs {
				w, err := a.host.Wins.Open(win.Spec{Cmd: "sleep 5", Kind: win.KindVSplit})
				if err != nil {
					t.Fatal(err)
				}
				_ = w.SetLayout(func(l *win.Layout) { l.Frac = f })
			}
			a.width, a.height = c.width, 30
			a.Frame()
			var wins []int
			for _, r := range a.winRects {
				wins = append(wins, r.w)
			}
			if a.stage.Dx() != c.wantMain || !slices.Equal(wins, c.wantWins) {
				t.Fatalf("main %d, windows %v; want %d, %v", a.stage.Dx(), wins, c.wantMain, c.wantWins)
			}
		})
	}
}

// TestSplitDragFollowsPointer: dragging a vsplit's separator or a split's
// title leaves it under the pointer, since frac is written against the
// cells the slot's box shares out.
func TestSplitDragFollowsPointer(t *testing.T) {
	for _, kind := range []string{win.KindVSplit, win.KindSplit} {
		t.Run(kind, func(t *testing.T) {
			a := newTestApp(t, t.TempDir())
			defer a.host.Wins.CloseAll()
			if _, err := a.host.Wins.Open(win.Spec{Cmd: "sleep 5", Kind: kind}); err != nil {
				t.Fatal(err)
			}
			a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
			a.Frame()
			edge := func() [2]int {
				r := a.winRects[0]
				if kind == win.KindVSplit {
					return [2]int{r.sepX, 10}
				}
				return [2]int{r.x + 2, r.titleY}
			}
			from := edge()
			to := [2]int{from[0] - 10, from[1]}
			if kind == win.KindSplit {
				to = [2]int{from[0], from[1] - 5}
			}
			a.Update(tea.MouseClickMsg{X: from[0], Y: from[1], Button: tea.MouseLeft})
			a.Update(tea.MouseMotionMsg{X: to[0], Y: to[1], Button: tea.MouseLeft})
			a.Update(tea.MouseReleaseMsg{X: to[0], Y: to[1], Button: tea.MouseLeft})
			a.Frame()
			if got := edge(); got != to {
				t.Fatalf("edge at %v after dragging from %v to %v", got, from, to)
			}
		})
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

// TestWidgetSizes: widgets render at their node's width, beside a vsplit,
// and a content-sized node's height is the most rows it could get, not its
// last draw: a widget padding to ctx.height would otherwise never shrink.
func TestWidgetSizes(t *testing.T) {
	a, _ := bootAgentApp(t, 120, 30)
	defer a.host.Wins.CloseAll()
	runLua(t, a, `zemi.ui.widget.register({ id = "head", region = "header", render = function(ctx)
		local rows = {}
		for i = 1, ctx.height do rows[i] = { "x" } end
		return rows
	end })`)
	if _, err := a.host.Wins.Open(win.Spec{Cmd: "sleep 5", Kind: win.KindVSplit}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		a.renderSync(t)
		a.Frame()
	}
	mainW := a.winRects[0].x - 1
	got := map[string][2]int{}
	for _, w := range a.shownWidgets() {
		got[w.ID] = [2]int{w.width, w.height}
	}
	want := map[string][2]int{"head": {mainW, 30}, "splash": {mainW, a.stage.Dy()}, "statusline.info": {mainW, 1}}
	for id, size := range want {
		if got[id] != size {
			t.Errorf("%s renders at %v, want %v (main column %d of 120)", id, got[id], size, mainW)
		}
	}
}

// TestWindowHostCursor: the focused window's cursor is the host cursor, in
// the shape the program asked for, and a float drawn over it hides it.
func TestWindowHostCursor(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.CaptureHooks()
	defer a.host.Wins.CloseAll()
	wn, err := a.host.Wins.Open(win.Spec{Cmd: `printf '\033[6 q\033[2;3H'; sleep 5`, Kind: win.KindFloat, Cols: 20, Rows: 6})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for scr := wn.Screen(); scr.CursorY != 1 || scr.CursorShape != 2; scr = wn.Screen() {
		if ctx.Err() != nil {
			t.Fatal("the cursor never moved")
		}
		time.Sleep(20 * time.Millisecond)
	}
	a.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	if a.View().Cursor != nil {
		t.Fatal("no window is focused, yet the host cursor shows")
	}
	a.setFocusWin(wn.ID)
	c := a.View().Cursor
	r := a.winRects[0]
	if c == nil || c.X != r.x+2 || c.Y != r.y+1 || c.Shape != tea.CursorBar || c.Blink {
		t.Fatalf("cursor %+v, window cells at %d,%d", c, r.x, r.y)
	}
	if _, err := a.host.Wins.Open(win.Spec{Cmd: "sleep 5", Kind: win.KindFloat, Cols: 20, Rows: 6}); err != nil {
		t.Fatal(err)
	}
	if c := a.View().Cursor; c != nil {
		t.Fatalf("a float covers the cursor, yet it shows at %+v", c)
	}
}

// TestFloatDrag: a float's title moves it and its border and corners resize
// it, kept at its minimum and on screen, and the program sees the new size.
// The float's box starts at 10,4 with 30x8 cells at 12,5: the left border is
// column 10, the right 43, the bottom row 13.
func TestFloatDrag(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.CaptureHooks()
	defer a.host.Wins.CloseAll()
	wn, err := a.host.Wins.Open(win.Spec{Cmd: `trap 'stty size' WINCH; echo ready; while :; do sleep 0.05; done`, Kind: win.KindFloat, Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if text, ok := wn.WaitText(ctx, "ready"); !ok {
		t.Fatalf("no trap:\n%s", text)
	}
	a.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	for _, c := range []struct {
		name     string
		from, to [2]int
		want     [4]int // x, y, cols, rows
	}{
		{"title moves", [2]int{20, 4}, [2]int{25, 6}, [4]int{15, 6, 30, 8}},
		{"left edge stops at the minimum width, right edge kept", [2]int{10, 8}, [2]int{70, 8}, [4]int{20, 4, 20, 8}},
		{"right edge stops at the screen", [2]int{42, 8}, [2]int{79, 8}, [4]int{10, 4, 66, 8}},
		{"top-left corner", [2]int{10, 4}, [2]int{5, 2}, [4]int{5, 2, 35, 10}},
		{"bottom stops at the minimum height", [2]int{20, 13}, [2]int{20, 0}, [4]int{10, 4, 30, 3}},
		{"bottom-right corner", [2]int{43, 13}, [2]int{48, 16}, [4]int{10, 4, 35, 11}},
	} {
		_ = wn.SetLayout(func(l *win.Layout) { l.X, l.Y, l.Cols, l.Rows = 10, 4, 30, 8 })
		a.Frame()
		a.Update(tea.MouseClickMsg{X: c.from[0], Y: c.from[1], Button: tea.MouseLeft})
		a.Update(tea.MouseMotionMsg{X: c.to[0], Y: c.to[1], Button: tea.MouseLeft})
		a.Update(tea.MouseReleaseMsg{X: c.to[0], Y: c.to[1], Button: tea.MouseLeft})
		if l := wn.Layout(); [4]int{l.X, l.Y, l.Cols, l.Rows} != c.want {
			t.Errorf("%s: layout %+v, want %v", c.name, l, c.want)
		}
	}
	a.Frame()
	if text, ok := wn.WaitText(ctx, "11 35"); !ok {
		t.Fatalf("the program never saw 35x11:\n%s", text)
	}
}

// TestTextWindowRenders: a text window draws its body through the chat's
// markdown renderer, a replacement shows in place, the text re-wraps when
// the window narrows and scrolling moves the view.
func TestTextWindowRenders(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	defer a.host.Wins.CloseAll()
	wn, err := a.host.Wins.OpenText(win.Spec{Text: "# Plan\n\n- one\n- two\n", Markdown: true, Kind: win.KindFloat})
	if err != nil {
		t.Fatal(err)
	}
	a.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	rows := func() [][]hostapi.Segment {
		a.Frame()
		i := slices.IndexFunc(a.winRects, func(r winRect) bool { return r.id == wn.ID })
		if i < 0 {
			t.Fatal("the text window was not drawn")
		}
		r := a.winRects[i]
		if r.w > 40 {
			t.Fatalf("a float with no size asked for is %d cells wide", r.w)
		}
		var out [][]hostapi.Segment
		for _, row := range a.lastRows[r.y : r.y+r.h] {
			out = append(out, truncateSegs(dropCells(row, r.x), r.w))
		}
		return out
	}
	body := func() string {
		var b strings.Builder
		for _, row := range rows() {
			b.WriteString(strings.TrimSpace(plainSegs(row)) + "\n")
		}
		return b.String()
	}
	if got := body(); !strings.Contains(got, "Plan") || strings.Contains(got, "# Plan") || !strings.Contains(got, "one") {
		t.Fatalf("markdown body:\n%s", got)
	}
	styled := false
	for _, row := range rows() {
		for _, s := range row {
			styled = styled || strings.HasPrefix(s.HL, hostapi.SGRPrefix)
		}
	}
	if !styled {
		t.Fatal("the renderer's colors did not reach the frame")
	}
	if err := wn.SetText("# Next\n\nthe body changed", false); err != nil {
		t.Fatal(err)
	}
	if got := body(); !strings.Contains(got, "Next") || strings.Contains(got, "Plan") {
		t.Fatalf("set_text did not replace the body:\n%s", got)
	}
	// narrow it: one long line has to come back as several
	if err := wn.SetLayout(func(l *win.Layout) { l.Cols, l.Rows = 24, 6 }); err != nil {
		t.Fatal(err)
	}
	if err := wn.SetText(strings.Repeat("word ", 20), false); err != nil {
		t.Fatal(err)
	}
	lines := 0
	for _, row := range rows() {
		if segsWidth(row) != 24 {
			t.Fatalf("row is %d cells, want 24", segsWidth(row))
		}
		if strings.Contains(plainSegs(row), "word") {
			lines++
		}
	}
	if lines < 4 {
		t.Fatalf("text wrapped to %d rows in 24 cells", lines)
	}
	// scrolling moves the view and stops at the last screenful
	wn.SetText("l1\n\nl2\n\nl3\n\nl4\n\nl5\n\nl6\n\nl7\n\nl8", false)
	first := strings.TrimSpace(plainSegs(rows()[0]))
	wn.Scroll(3)
	if scrolled := strings.TrimSpace(plainSegs(rows()[0])); scrolled == first {
		t.Fatalf("scrolling kept the view at %q", first)
	}
	wn.Scroll(1000)
	if got := body(); !strings.Contains(got, "l8") {
		t.Fatalf("the end of the text is not reachable:\n%s", got)
	}
}
