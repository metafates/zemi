// win.go draws windows (internal/win): the layout tree's slots place vsplits
// and splits (layout.go), floats sit over everything.
// A focused window gets every key, the unfocus key (ui.window.unfocus)
// gives them back; a click on a window focuses it, its × closes it, and
// dragging a float's title moves it, a float's border or corner, a vsplit's
// separator or a split's title resizes it.
package ui

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/win"
)

// winRect is where one window's cells landed on the last frame, plus its
// title row and, for a vsplit, the separator column left of it.
type winRect struct {
	id         int
	x, y, w, h int // the terminal cells
	titleY     int
	sepX       int // vsplit: the separator column beside it, left or right, -1 when none
	axis       int // split, vsplit: the cells its frac is a share of
	float      bool
}

// winDrag is a mouse drag in progress on a window's chrome.
type winDrag struct {
	id     int
	what   string // "move", "resize", "vsplit", "split" or "title" (a press that may become a drag)
	edge   string // resize: the float's edges that follow the pointer (floatEdge)
	x0, y0 int    // where the press landed
	r0     winRect
	moved  bool
}

const (
	minVsplitW = 20
	minSplitH  = 4
	minFloatW  = 20
	minFloatH  = 3
)

// shownWins lists the windows of one kind that are not hidden, in opening
// order.
func (a *App) shownWins(kind string) []*win.Window {
	var out []*win.Window
	for _, wn := range a.host.Wins.List() {
		if l := wn.Layout(); l.Kind == kind && !l.Hidden {
			out = append(out, wn)
		}
	}
	return out
}

// floatBox frames a float and places it: cols and rows are what it asked
// for, capped to the frame; x, y its top-left, centered over the stage
// when unset. It reports the frame's origin and the terminal cells' rect.
func (a *App) floatBox(wn *win.Window, w, h int, stage uv.Rectangle) (box [][]hostapi.Segment, x0, y0 int, r winRect) {
	headerH, chatH := stage.Min.Y, stage.Dy()
	l := wn.Layout()
	cols, rows := l.Cols, l.Rows
	if cols <= 0 {
		cols = w * 4 / 5
	}
	if rows <= 0 {
		rows = chatH * 4 / 5
	}
	if wn.Owner == "editor" && a.focusWin != wn.ID {
		rows = min(rows, chatH-2) // an editor left for zemi uncovers the composer it gave the keys to
	}
	cols, rows = max(min(cols, w-4), 2), max(min(rows, h-3), 1)
	wn.Resize(cols, rows)
	// the title row goes into the top border: one frame, not two
	body := a.windowRows(wn, cols, rows)
	top := []hostapi.Segment{{Text: "╭─"}}
	for _, s := range body[0] {
		switch {
		case strings.TrimSpace(s.Text) == "":
			s.Text, s.HL = strings.Repeat("─", ansi.StringWidth(s.Text)), ""
		case s.HL == "Host:Panel":
			s.HL = ""
		case s.HL == "Host:PanelTitle":
			s.HL = "Host:Title"
		}
		top = append(top, s)
	}
	box = [][]hostapi.Segment{append(top, hostapi.Segment{Text: "─╮"})}
	bw := cols + 4
	for _, row := range body[1:] {
		line := []hostapi.Segment{{Text: "│ "}}
		line = append(line, row...)
		box = append(box, append(line, hostapi.Segment{Text: " │"}))
	}
	box = append(box, []hostapi.Segment{{Text: "╰" + strings.Repeat("─", bw-2) + "╯"}})
	x0, y0 = l.X, l.Y
	if x0 < 0 {
		x0 = max((w-bw)/2, 0)
	}
	if y0 < 0 {
		y0 = headerH + max((chatH-len(box))/2, 0)
	}
	x0, y0 = min(x0, max(w-bw, 0)), min(y0, max(h-len(box), 0))
	return box, x0, y0, winRect{id: wn.ID, x: x0 + 2, y: y0 + 1, w: cols, h: rows, titleY: y0, sepX: -1, float: true}
}

// placeBox writes box at x0, y0 over rows, clipped to the frame.
func placeBox(rows, box [][]hostapi.Segment, x0, y0, w int) [][]hostapi.Segment {
	for i, line := range box {
		y := y0 + i
		if y < 0 || y >= len(rows) {
			continue
		}
		bw := segsWidth(line)
		under := rows[y]
		row := padSegs(truncateSegs(under, x0), x0)
		row = append(row, line...)
		row = append(row, dropCells(under, x0+bw)...)
		rows[y] = padSegs(truncateSegs(row, w), w)
	}
	return rows
}

// windowRows draws one window: a title row, then its screen, every row
// padded to width. Rows the screen lacks come out blank.
func (a *App) windowRows(wn *win.Window, width, height int) [][]hostapi.Segment {
	focused := a.focusWin == wn.ID
	rows := [][]hostapi.Segment{a.winTitleRow(wn, width, focused)}
	scr := wn.Screen()
	if focused && scr.HasCursor {
		a.cursor = &tea.Cursor{Position: tea.Position{X: scr.CursorX, Y: scr.CursorY}, Shape: tea.CursorShape(scr.CursorShape), Blink: scr.CursorBlink}
	}
	for y := range height {
		var row []hostapi.Segment
		if y < len(scr.Rows) {
			for _, sp := range scr.Rows[y] {
				hl := ""
				if sp.SGR != "" {
					hl = hostapi.SGRPrefix + sp.SGR
				}
				// placeholders survive: Screen keeps only the window's own images
				parts := strings.Split(sp.Text, string(kitty.Placeholder))
				for i, p := range parts {
					parts[i] = sanitize.String(p)
				}
				row = append(row, hostapi.Segment{Text: strings.Join(parts, string(kitty.Placeholder)), HL: hl})
			}
		}
		row = padSegs(truncateSegs(row, width), width)
		rows = append(rows, row)
	}
	return rows
}

// placeCursor moves the focused window's cursor from its cells to the
// frame; nil when a modal is up or the cell is off the window or under a
// float drawn after it.
func (a *App) placeCursor(c *tea.Cursor, modal bool) *tea.Cursor {
	i := slices.IndexFunc(a.winRects, func(r winRect) bool { return r.id == a.focusWin })
	if c == nil || modal || i < 0 {
		return nil
	}
	r := a.winRects[i]
	if c.X >= r.w || c.Y >= r.h {
		return nil
	}
	c.X, c.Y = r.x+c.X, r.y+c.Y
	for _, o := range a.winRects[i+1:] {
		// a float's box is its cells plus a two-column border and the title and bottom rows
		if o.float && c.X >= o.x-2 && c.X < o.x+o.w+2 && c.Y >= o.titleY && c.Y <= o.y+o.h {
			return nil
		}
	}
	return c
}

// winTitleRow is the row above a window: id and title on the left, state
// and the close mark on the right. Focus lights the title, not the bar.
func (a *App) winTitleRow(wn *win.Window, width int, focused bool) []hostapi.Segment {
	hl, titleHL := "Host:Panel", "Host:Panel"
	if focused {
		titleHL = "Host:PanelTitle"
	}
	state, closeMark := "", " × "
	switch {
	case wn.Exited() && focused:
		state = fmt.Sprintf("exited %d · any key closes", wn.ExitCode())
	case wn.Exited():
		state = fmt.Sprintf("exited %d", wn.ExitCode())
	case focused:
		state = a.unfocusKey() + " to leave"
	case a.mouseEnabled():
		state = "click to focus"
	default: // no clicks reach zemi
		state, closeMark = "/win to focus", " "
	}
	left := fmt.Sprintf(" %d %s ", wn.ID, sanitize.String(wn.Title()))
	right := " " + state + closeMark
	if ansi.StringWidth(right) > width-4 {
		right = closeMark // a narrow window keeps its close mark where titleClick looks
	}
	right = ansi.TruncateLeft(right, max(0, ansi.StringWidth(right)-width), "")
	left = ansi.Truncate(left, max(0, width-ansi.StringWidth(right)), "…")
	fill := max(0, width-ansi.StringWidth(left)-ansi.StringWidth(right))
	return []hostapi.Segment{
		{Text: left, HL: titleHL},
		{Text: strings.Repeat(" ", fill), HL: hl},
		{Text: right, HL: hl},
	}
}

func (a *App) unfocusKey() string {
	if k := a.cfgStr("ui.window.unfocus"); k != "" {
		return k
	}
	return `ctrl+\`
}

// focusedWin is the window taking keys, nil when none (a closed or hidden
// window drops the focus).
func (a *App) focusedWin() *win.Window {
	if a.focusWin == 0 {
		return nil
	}
	wn, ok := a.host.Wins.Get(a.focusWin)
	if !ok || wn.Hidden() {
		a.focusWin = 0
		return nil
	}
	return wn
}

// setFocusWin moves the focus and tells both programs.
func (a *App) setFocusWin(id int) {
	if id == a.focusWin {
		return
	}
	if old, ok := a.host.Wins.Get(a.focusWin); ok {
		old.Focus(false)
	}
	a.focusWin = id
	if wn, ok := a.host.Wins.Get(id); ok {
		wn.Focus(true)
	}
}

// windowKey routes a key to the focused window; it reports whether one
// took it. An exited window closes on any key.
func (a *App) windowKey(key string, msg uv.KeyPressEvent) bool {
	wn := a.focusedWin()
	if wn == nil {
		return false
	}
	switch {
	case key == a.unfocusKey():
		a.setFocusWin(0)
	case wn.Exited():
		a.host.CloseWindow(wn.ID)
		a.focusWin = 0
	default:
		wn.SendKey(msg)
	}
	return true
}

// windowMouse handles a mouse event on window chrome or cells and reports
// whether one took it. Chrome: a press on a title, separator or float border
// starts a drag (move a float, resize a float, vsplit or split); a release
// without motion on a title focuses, on its × closes. Cells: a click focuses and the
// event is forwarded; the wheel goes to the window under the pointer,
// motion only to the focused one.
func (a *App) windowMouse(msg tea.Msg) bool {
	var m tea.Mouse
	switch e := msg.(type) {
	case tea.MouseClickMsg:
		m = tea.Mouse(e)
	case tea.MouseReleaseMsg:
		m = tea.Mouse(e)
	case tea.MouseMotionMsg:
		m = tea.Mouse(e)
	case tea.MouseWheelMsg:
		m = tea.Mouse(e)
	default:
		return false
	}
	if d := a.winDrag; d != nil {
		switch msg.(type) {
		case tea.MouseMotionMsg:
			a.dragWindow(d, m.X, m.Y)
		case tea.MouseReleaseMsg:
			a.winDrag = nil
			if !d.moved && d.what == "title" {
				a.titleClick(d.id, m.X)
			}
		}
		return true
	}
	// last drawn is on top: floats cover splits and vsplits
	for _, r := range slices.Backward(a.winRects) {
		onTitle := m.Y == r.titleY && m.X >= r.x && m.X < r.x+r.w
		onSep := r.sepX >= 0 && m.X == r.sepX && m.Y >= r.titleY && m.Y < r.y+r.h
		edge := floatEdge(r, m.X, m.Y)
		if _, click := msg.(tea.MouseClickMsg); click && m.Button == tea.MouseLeft && (onTitle || onSep || edge != "") {
			d := &winDrag{id: r.id, x0: m.X, y0: m.Y, what: "title", edge: edge, r0: r}
			switch {
			case onSep:
				d.what = "vsplit"
			case edge != "":
				d.what = "resize"
			}
			a.winDrag = d
			return true
		}
		if m.Y < r.y || m.Y >= r.y+r.h || m.X < r.x || m.X >= r.x+r.w {
			continue
		}
		wn, ok := a.host.Wins.Get(r.id)
		if !ok {
			return false
		}
		local := uv.Mouse{X: m.X - r.x, Y: m.Y - r.y, Button: m.Button, Mod: m.Mod}
		switch msg.(type) {
		case tea.MouseClickMsg:
			a.setFocusWin(r.id)
			wn.SendMouse(uv.MouseClickEvent(local))
		case tea.MouseReleaseMsg:
			wn.SendMouse(uv.MouseReleaseEvent(local))
		case tea.MouseWheelMsg:
			wn.SendMouse(uv.MouseWheelEvent(local))
		case tea.MouseMotionMsg:
			if a.focusWin == r.id {
				wn.SendMouse(uv.MouseMotionEvent(local))
			}
		}
		return true
	}
	if _, click := msg.(tea.MouseClickMsg); click && a.focusWin != 0 {
		a.setFocusWin(0) // a click anywhere else is for zemi
	}
	return false
}

// titleClick is a click on a window's title row: its × closes, the rest
// focuses.
func (a *App) titleClick(id, x int) {
	for _, r := range a.winRects {
		if r.id != id {
			continue
		}
		if x >= r.x+r.w-3 {
			if err := a.host.CloseWindow(id); err != nil {
				a.pushToast("warn", "Window", err.Error())
				return
			}
			if a.focusWin == id {
				a.focusWin = 0
			}
			return
		}
		a.setFocusWin(id)
	}
}

// dragWindow applies a drag: a float moves by the pointer's travel or its
// dragged edges follow it, kept on screen and at least minFloatW by minFloatH;
// a vsplit's width follows the separator, a split's height its title. The
// × corner never starts a drag, so a slipped click still closes.
func (a *App) dragWindow(d *winDrag, x, y int) {
	wn, ok := a.host.Wins.Get(d.id)
	if !ok {
		a.winDrag = nil
		return
	}
	var r winRect
	for _, c := range a.winRects {
		if c.id == d.id {
			r = c
		}
	}
	if x == d.x0 && y == d.y0 {
		return
	}
	if d.what == "title" {
		if d.x0 >= r.x+r.w-3 {
			return
		}
		d.what = map[string]string{win.KindSplit: "split", win.KindVSplit: "vsplit"}[wn.Layout().Kind]
		if r.float { // a split with no slot floats too
			d.what = "move"
		}
	}
	d.moved = true
	_ = wn.SetLayout(func(l *win.Layout) {
		switch d.what {
		case "move":
			l.X, l.Y = max(0, d.r0.x-2+x-d.x0), max(0, d.r0.titleY+y-d.y0)
		case "resize":
			// the box is the cells plus two columns each side and a row above and below
			bx, by, cols, rows := d.r0.x-2, d.r0.titleY, d.r0.w, d.r0.h
			if strings.Contains(d.edge, "r") {
				cols = min(max(cols+x-d.x0, minFloatW), a.width-bx-4)
			}
			if strings.Contains(d.edge, "l") {
				dx := max(min(x-d.x0, max(cols-minFloatW, 0)), -bx)
				bx, cols = bx+dx, cols-dx
			}
			if strings.Contains(d.edge, "b") {
				rows = min(max(rows+y-d.y0, minFloatH), a.height-by-2)
			}
			if strings.Contains(d.edge, "t") {
				dy := max(min(y-d.y0, max(rows-minFloatH, 0)), -by)
				by, rows = by+dy, rows-dy
			}
			l.X, l.Y, l.Cols, l.Rows = bx, by, cols, rows
		case "vsplit":
			cols := r.x + r.w - x - 1
			if r.sepX >= r.x+r.w { // the separator is on its right
				cols = x - r.x
			}
			l.Frac = float64(cols) / float64(max(r.axis, 1))
		case "split":
			l.Frac = float64(r.y+r.h-y) / float64(max(r.axis, 1))
		}
	})
}

// floatEdge names the edges of float r's border at (x, y) that a drag
// there resizes, "t", "b", "l" and "r" joined ("bl" for a corner); "" off the
// border. The title row between the corners moves the float instead.
func floatEdge(r winRect, x, y int) string {
	if !r.float || x < r.x-2 || x > r.x+r.w+1 || y < r.titleY || y > r.y+r.h {
		return ""
	}
	e := ""
	switch {
	case y == r.titleY && (x < r.x || x >= r.x+r.w):
		e = "t"
	case y == r.y+r.h:
		e = "b"
	}
	switch {
	case x < r.x:
		e += "l"
	case x >= r.x+r.w:
		e += "r"
	}
	return e
}

// edgeShape is the OSC 22 pointer for a floatEdge, "" for none.
func (a *App) edgeShape(x, y int) string {
	for _, r := range slices.Backward(a.winRects) {
		if e := floatEdge(r, x, y); e != "" {
			return map[string]string{"l": "ew-resize", "r": "ew-resize", "b": "ns-resize",
				"tl": "nwse-resize", "br": "nwse-resize", "tr": "nesw-resize", "bl": "nesw-resize"}[e]
		}
		if y >= r.titleY && y < r.y+r.h && x >= r.x && x < r.x+r.w {
			return "" // a window drawn over the border
		}
	}
	return ""
}

// FocusWindow implements hostapi.UIHooks.
func (a *App) FocusWindow(id int) { a.do(func() { a.setFocusWin(id) }) }
