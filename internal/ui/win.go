// win.go lays out and draws windows (internal/win): vsplits are full-height
// columns beside the main column (header, chat, composer, statusline),
// splits take rows at the bottom of the chat, floats sit over everything.
// A focused window gets every key, the unfocus key (ui.window.unfocus)
// gives them back; a click on a window focuses it, its × closes it, and
// dragging a float's title moves it, a vsplit's separator or a split's
// title resizes it.
// NOTE: the layout is a fixed order (main, vsplits left to right, splits
// top to bottom), not a tree; zellij-style nesting is the upgrade path.
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
	sepX       int // vsplit: the separator column, -1 otherwise
}

// winDrag is a mouse drag in progress on a window's chrome.
type winDrag struct {
	id     int
	what   string // "move", "vsplit", "split" or "title" (a press that may become a drag)
	x0, y0 int    // where the press landed
	fx, fy int    // float: position when the drag began
	moved  bool
}

const (
	minVsplitW = 20
	minSplitH  = 4
	minMainW   = 30
	minChatH   = 3
)

// winColumns decides the vsplit columns for a frame of width w: mainW is
// what the main column keeps.
func (a *App) winColumns(w int) (vsplits []*win.Window, widths []int, mainW int) {
	vsplits = a.shownWins(win.KindVSplit)
	widths = shares(vsplits, w-len(vsplits), minVsplitW, minMainW)
	mainW = w - len(vsplits)
	for _, x := range widths {
		mainW -= x
	}
	return vsplits, widths, mainW
}

// shares divides total cells between the windows and the main area: a
// window with a Frac gets that share, the rest split what is left with
// the main area equally, and the main area keeps at least minMain.
func shares(wins []*win.Window, total, minWin, minMain int) []int {
	if len(wins) == 0 {
		return nil
	}
	out := make([]int, len(wins))
	fixed, auto := 0, 0
	for i, wn := range wins {
		if f := wn.Layout().Frac; f > 0 {
			out[i] = max(minWin, int(f*float64(total)))
			fixed += out[i]
		} else {
			auto++
		}
	}
	each := max(minWin, (total-fixed-minMain)/(auto+1))
	for i := range wins {
		if out[i] == 0 {
			out[i] = each
		}
	}
	// squeeze windows, last first, until the main area fits
	for i := len(out) - 1; i >= 0; i-- {
		sum := 0
		for _, x := range out {
			sum += x
		}
		if over := sum - (total - minMain); over > 0 {
			out[i] = max(minWin, out[i]-over)
		}
	}
	return out
}

// winRowsLayout decides the splits inside a chat area chatH rows tall:
// chatTop is what the chat viewport keeps.
func (a *App) winRowsLayout(chatH int) (splits []*win.Window, heights []int, chatTop int) {
	splits = a.shownWins(win.KindSplit)
	heights = shares(splits, chatH, minSplitH, minChatH)
	chatTop = chatH
	for _, x := range heights {
		chatTop -= x
	}
	return splits, heights, max(chatTop, 1)
}

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
// for, capped to the frame; x, y its top-left, centered over the chat
// when unset. It reports the frame's origin and the terminal cells' rect.
func (a *App) floatBox(wn *win.Window, w, h, headerH, chatH int) (box [][]hostapi.Segment, x0, y0 int, r winRect) {
	l := wn.Layout()
	cols, rows := l.Cols, l.Rows
	if cols <= 0 {
		cols = w * 4 / 5
	}
	if rows <= 0 {
		rows = chatH * 4 / 5
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
	return box, x0, y0, winRect{id: wn.ID, x: x0 + 2, y: y0 + 1, w: cols, h: rows, titleY: y0, sepX: -1}
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
		if focused && scr.HasCursor && y == scr.CursorY && scr.CursorX < width {
			row = restyle(row, scr.CursorX, scr.CursorX+1, func(string) string { return "Host:Cursor" })
		}
		rows = append(rows, row)
	}
	return rows
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
// whether one took it. Chrome: a press on a title or separator starts a
// drag (move a float, resize a vsplit or split); a release without motion
// on a title focuses, on its × closes. Cells: a click focuses and the
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
		if _, click := msg.(tea.MouseClickMsg); click && m.Button == tea.MouseLeft && (onTitle || onSep) {
			d := &winDrag{id: r.id, x0: m.X, y0: m.Y, what: "title"}
			if onSep {
				d.what = "vsplit"
			}
			if wn, ok := a.host.Wins.Get(r.id); ok {
				l := wn.Layout()
				d.fx, d.fy = l.X, l.Y
				if l.X < 0 || l.Y < 0 { // centered: pin where it is
					d.fx, d.fy = r.x-2, r.titleY
				}
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

// dragWindow applies a drag: a float moves by the pointer's travel, a
// vsplit's width follows the separator, a split's height its title. The
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
		d.what = map[string]string{win.KindFloat: "move", win.KindSplit: "split", win.KindVSplit: "vsplit"}[wn.Layout().Kind]
	}
	d.moved = true
	_ = wn.SetLayout(func(l *win.Layout) {
		switch d.what {
		case "move":
			l.X, l.Y = max(0, d.fx+x-d.x0), max(0, d.fy+y-d.y0)
		case "vsplit":
			l.Frac = float64(r.x+r.w-x-1) / float64(max(a.width, 1))
		case "split":
			l.Frac = float64(r.y+r.h-y) / float64(max(a.chatAreaH, 1))
		}
	})
}

// FocusWindow implements hostapi.UIHooks.
func (a *App) FocusWindow(id int) { a.do(func() { a.setFocusWin(id) }) }
