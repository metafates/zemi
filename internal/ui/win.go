// win.go lays out and draws windows (internal/win) over the chat area:
// vsplits take columns on the right, splits take rows at the bottom, floats
// sit centered on top. A focused window gets every key, the unfocus key
// (ui.window.unfocus) gives them back; a click on a window focuses it, on
// its × closes it, anywhere else unfocuses.
// NOTE: the layout is a fixed order (chat, vsplits, splits), not a tree;
// zellij-style nesting and dragging borders are the upgrade path.
package ui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/win"
)

// winRect is where one window's cells landed on the last frame, plus the
// title row above them.
type winRect struct {
	id         int
	x, y, w, h int // the terminal cells
	titleY     int
}

// winLayout is one frame's window arrangement.
type winLayout struct {
	vsplits, splits, floats []*win.Window
	chatW, chatTopH         int // what the chat keeps
	vw, sh                  int // one vsplit's width, one split's height (title row included)
}

const (
	minVsplitW = 20
	minSplitH  = 4
)

// layoutWindows decides how the chat area is shared and resizes each window
// to its cells. chatH is the whole area's height.
func (a *App) layoutWindows(w, chatH int) winLayout {
	l := winLayout{chatW: w, chatTopH: chatH}
	for _, wn := range a.host.Wins.List() {
		switch wn.Kind {
		case win.KindVSplit:
			l.vsplits = append(l.vsplits, wn)
		case win.KindFloat:
			l.floats = append(l.floats, wn)
		default:
			l.splits = append(l.splits, wn)
		}
	}
	if nv := len(l.vsplits); nv > 0 {
		l.vw = max(minVsplitW, (w-nv)/(nv+1))
		l.chatW = max(minVsplitW, w-nv*(l.vw+1))
	}
	if ns := len(l.splits); ns > 0 {
		l.sh = max(minSplitH, chatH/(ns+1))
		l.chatTopH = max(1, chatH-ns*l.sh)
	}
	for _, wn := range l.vsplits {
		wn.Resize(l.vw, l.chatTopH-1)
	}
	for _, wn := range l.splits {
		wn.Resize(w, l.sh-1)
	}
	for _, wn := range l.floats {
		cols, rows := floatSize(wn, w, chatH)
		wn.Resize(cols, rows)
	}
	return l
}

// floatSize is a float's terminal size: what it asked for, capped to the
// chat area with room for the frame, else four fifths of it.
func floatSize(wn *win.Window, w, chatH int) (cols, rows int) {
	cols, rows = wn.Cols, wn.Rows
	if cols <= 0 {
		cols = w * 4 / 5
	}
	if rows <= 0 {
		rows = chatH * 4 / 5
	}
	return max(min(cols, w-4), 2), max(min(rows, chatH-2), 1)
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
				row = append(row, hostapi.Segment{Text: sanitize.String(sp.Text), HL: hl})
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

// windowTitle is the row above a window: id and title on the left, state
// and the close mark on the right.
func (a *App) winTitleRow(wn *win.Window, width int, focused bool) []hostapi.Segment {
	hl := "Host:Panel"
	if focused {
		hl = "Host:BtnSel"
	}
	state := ""
	switch {
	case wn.Exited():
		state = fmt.Sprintf("exited %d · any key closes", wn.ExitCode())
	case focused:
		state = a.unfocusKey() + " leaves"
	default:
		state = "click to type"
	}
	left := fmt.Sprintf(" %d %s ", wn.ID, sanitize.String(wn.Title()))
	right := " " + state + " × "
	left = ansi.Truncate(left, max(0, width-ansi.StringWidth(right)), "…")
	fill := max(0, width-ansi.StringWidth(left)-ansi.StringWidth(right))
	return []hostapi.Segment{
		{Text: left, HL: hl},
		{Text: strings.Repeat(" ", fill), HL: hl},
		{Text: right, HL: hl},
	}
}

// floatBox frames a float window like the host boxes.
func (a *App) floatBox(wn *win.Window, cols, rows int) [][]hostapi.Segment {
	body := a.windowRows(wn, cols, rows)
	bw := cols + 4
	out := [][]hostapi.Segment{{{Text: "╭" + strings.Repeat("─", bw-2) + "╮"}}}
	for _, r := range body {
		row := []hostapi.Segment{{Text: "│ "}}
		row = append(row, r...)
		out = append(out, append(row, hostapi.Segment{Text: " │"}))
	}
	return append(out, []hostapi.Segment{{Text: "╰" + strings.Repeat("─", bw-2) + "╯"}})
}

func (a *App) unfocusKey() string {
	if k := a.cfgStr("ui.window.unfocus"); k != "" {
		return k
	}
	return `ctrl+\`
}

// focusedWin is the window taking keys, nil when none (a closed window
// drops the focus).
func (a *App) focusedWin() *win.Window {
	if a.focusWin == 0 {
		return nil
	}
	wn, ok := a.host.Wins.Get(a.focusWin)
	if !ok {
		a.focusWin = 0
		return nil
	}
	return wn
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
		a.focusWin = 0
	case wn.Exited():
		a.host.CloseWindow(wn.ID)
		a.focusWin = 0
	default:
		wn.SendKey(msg)
	}
	return true
}

// windowMouse forwards a mouse event landing on a window's cells and
// reports whether one took it: a click focuses the window first, the
// wheel goes to whichever window is under the pointer, motion only to
// the focused one (the others would drown in it).
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
	for _, r := range a.winRects {
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
			a.focusWin = r.id
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
	return false
}

// windowClick handles a click at (x, y) against the last frame's windows:
// the × closes, the rest of a window focuses it, anywhere else unfocuses.
// It reports whether a window took the click.
func (a *App) windowClick(x, y int) bool {
	for _, r := range a.winRects {
		inTitle := y == r.titleY && x >= r.x && x < r.x+r.w
		inCells := y >= r.y && y < r.y+r.h && x >= r.x && x < r.x+r.w
		if !inTitle && !inCells {
			continue
		}
		if inTitle && x >= r.x+r.w-3 {
			a.host.CloseWindow(r.id)
			if a.focusWin == r.id {
				a.focusWin = 0
			}
			return true
		}
		a.focusWin = r.id
		return true
	}
	if a.focusWin != 0 {
		a.focusWin = 0
	}
	return false
}

// FocusWindow implements hostapi.UIHooks.
func (a *App) FocusWindow(id int) { a.do(func() { a.focusWin = id }) }
