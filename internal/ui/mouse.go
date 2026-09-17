// mouse.go: a drag selects screen text and copies it on release (OSC 52
// plus the native clipboard tool); a click without a drag acts on what is
// under it: prompt choices, list rows, widget click ids, the thought line.
// Hovering lights what a click would act on.
package ui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/win"
)

// mousePress is the last button press, for the event a widget on_click gets.
type mousePress struct {
	x, y   int
	button tea.MouseButton
	mod    tea.KeyMod
	at     time.Time
	clicks int
}

type mouseSel struct {
	pressed, dragged bool
	x0, y0, x1, y1   int
}

// span is the selected cell range [from, to) of screen row y.
func (s mouseSel) span(y, w int) (from, to int, ok bool) {
	if !s.dragged {
		return 0, 0, false
	}
	ay, ax, by, bx := s.y0, s.x0, s.y1, s.x1
	if by < ay || (by == ay && bx < ax) {
		ay, ax, by, bx = by, bx, ay, ax
	}
	if y < ay || y > by {
		return 0, 0, false
	}
	from, to = 0, w
	if y == ay {
		from = ax
	}
	if y == by {
		to = bx + 1
	}
	return from, to, to > from
}

func (a *App) mouseEnabled() bool { return a.cfgBool("ui.mouse", true) }

func (a *App) handleMouse(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case tea.MouseClickMsg:
		p := mousePress{x: m.X, y: m.Y, button: m.Button, mod: m.Mod, at: time.Now(), clicks: 1}
		// NOTE: a fixed 400ms, not the OS double-click time
		if l := a.press; l.x == p.x && l.y == p.y && l.button == p.button && p.at.Sub(l.at) < 400*time.Millisecond {
			p.clicks = l.clicks + 1
		}
		a.press = p
		if m.Button != tea.MouseLeft {
			// right and middle presses start no selection: they click a widget at once
			if (m.Button == tea.MouseRight || m.Button == tea.MouseMiddle) && m.Y >= 0 && m.Y < len(a.lastRows) {
				if rest := dropCells(a.lastRows[m.Y], m.X); len(rest) > 0 && rest[0].Widget != "" {
					a.click(m.X, m.Y)
				}
			}
			return nil
		}
		a.mouse = mouseSel{pressed: true, x0: m.X, y0: m.Y, x1: m.X, y1: m.Y}
		a.frameGen++
	case tea.MouseMotionMsg:
		if !a.mouse.pressed {
			return a.hover(m.X, m.Y)
		}
		if m.X != a.mouse.x1 || m.Y != a.mouse.y1 {
			a.mouse.x1, a.mouse.y1, a.mouse.dragged = m.X, m.Y, true
			a.frameGen++
		}
	case tea.MouseReleaseMsg:
		if !a.mouse.pressed {
			return nil
		}
		a.mouse.pressed = false
		a.frameGen++
		if !a.mouse.dragged {
			a.mouse = mouseSel{}
			a.focusAt(m.X, m.Y)
			a.click(m.X, m.Y)
			return a.hover(m.X, m.Y)
		}
		text := a.selectionText()
		if text == "" {
			a.mouse = mouseSel{}
			return nil
		}
		a.pushToast("info", "Copied to the clipboard", "")
		return copyCmd(text)
	}
	return nil
}

// focusAt gives the keys to the focusable node a click lands in (the topmost:
// floats come last), or to the composer when it lands there.
func (a *App) focusAt(x, y int) {
	if a.topChrome() != "" {
		return
	}
	in := func(r hostapi.Rect) bool { return x >= r.X && x < r.X+r.Width && y >= r.Y && y < r.Y+r.Height }
	if in(a.rects["composer"]) {
		a.host.SetFocus("")
		return
	}
	for _, id := range slices.Backward(a.focusables) {
		if in(a.rects[id]) {
			a.host.SetFocus(id)
			return
		}
	}
}

// selectionText is the plain text under the selection, one line per row.
func (a *App) selectionText() string {
	var lines []string
	for y, row := range a.lastRows {
		if from, to, ok := a.mouse.span(y, a.width); ok {
			lines = append(lines, strings.TrimRight(win.StripPlaceholders(plainSegs(truncateSegs(dropCells(row, from), to-from))), " "))
		}
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// highlightSelection restyles the selected cells of the composed rows.
func (a *App) highlightSelection(rows [][]hostapi.Segment, w int) {
	for y := range rows {
		if from, to, ok := a.mouse.span(y, w); ok {
			rows[y] = restyle(rows[y], from, to, func(string) string { return "Host:Selection" })
		}
	}
}

// restyle maps the highlight of cells [from, to) of a row.
func restyle(row []hostapi.Segment, from, to int, hl func(string) string) []hostapi.Segment {
	mid := truncateSegs(dropCells(row, from), to-from)
	mid = append([]hostapi.Segment(nil), mid...)
	for i := range mid {
		if !strings.ContainsRune(mid[i].Text, kitty.Placeholder) { // its color names the image
			mid[i].HL = hl(mid[i].HL)
		}
	}
	out := append(append([]hostapi.Segment{}, truncateSegs(row, from)...), mid...)
	return append(out, dropCells(row, to)...)
}

// highlightHover lights what a click under the pointer would act on.
func (a *App) highlightHover(rows [][]hostapi.Segment) {
	if !a.hovering || a.mouse.pressed || a.mouse.dragged {
		return
	}
	spans, _, ok := a.target(a.hoverX, a.hoverY)
	if !ok {
		return
	}
	for _, sp := range spans {
		rows[sp.y] = restyle(rows[sp.y], sp.lo, sp.hi, func(string) string { return "Host:Hover" })
	}
}

// hover tracks the pointer; the frame redraws only when what it points at
// changes, and supporting terminals show a hand over clickable things.
func (a *App) hover(x, y int) tea.Cmd {
	a.hovering, a.hoverX, a.hoverY = true, x, y
	spans, _, ok := a.target(x, y)
	key := ""
	if ok {
		key = fmt.Sprint(spans)
	}
	if key != a.hoverKey {
		a.hoverKey = key
		a.frameGen++
	}
	shape := a.edgeShape(x, y)
	if shape == "" && ok {
		shape = "pointer"
	}
	return a.setPointer(shape)
}

// setPointer sets the pointer shape with OSC 22, "" for the default, when it
// changed; terminals without OSC 22 ignore the sequence.
func (a *App) setPointer(shape string) tea.Cmd {
	shape = cmp.Or(shape, "default")
	if shape == a.pointer {
		return nil
	}
	a.pointer = shape
	return tea.Raw("\x1b]22;" + shape + "\x07")
}

// click acts on the topmost thing under (x, y).
func (a *App) click(x, y int) {
	if _, act, ok := a.target(x, y); ok {
		act()
		a.publishComposer()
	}
	a.frameGen++
}

// cellSpan is the cells [lo, hi) of screen row y.
type cellSpan struct{ y, lo, hi int }

// target finds what a click at (x, y) would do: the cells that act, over
// every row of the item, and the action. Clicks and hover share it, so a
// highlight always means a click does something. The pointer must be over
// the item's own cells on its row: padding, gutters and blank rows are not
// targets. A list item is selected by the first click and opened by a click
// on the selected item.
func (a *App) target(x, y int) (spans []cellSpan, act func(), ok bool) {
	if y < 0 || y >= len(a.lastRows) {
		return nil, nil, false
	}
	row := a.lastRows[y]
	var under hostapi.Segment
	if rest := dropCells(row, x); len(rest) > 0 {
		under = rest[0]
	}
	hit := under.Hit
	// at is the item of the segments same matches, and whether x is on it
	at := func(same func(hostapi.Segment) bool) ([]cellSpan, bool) {
		spans := a.itemSpans(y, same)
		return spans, slices.ContainsFunc(spans, func(s cellSpan) bool { return s.y == y && s.lo <= x && x < s.hi })
	}
	item := func(n int) ([]cellSpan, bool) {
		if hit <= 0 || hit > n {
			return nil, false
		}
		return at(func(s hostapi.Segment) bool { return s.Hit == hit })
	}
	// with nothing to open, the selected item is not a target
	list := func(n int, sel *int, open func()) ([]cellSpan, func(), bool) {
		spans, ok := item(n)
		if !ok || (open == nil && *sel == hit-1) {
			return nil, nil, false
		}
		return spans, func() {
			if *sel == hit-1 {
				open()
			} else {
				*sel = hit - 1
			}
		}, true
	}
	switch top := a.topChrome(); {
	case top == "perm":
		pa := a.permAsk
		choices := pa.choices()
		spans, ok := item(len(choices))
		if !ok || (a.program != nil && a.now().Sub(pa.opened) < permGrace) {
			return nil, nil, false
		}
		c := choices[hit-1]
		return spans, func() { a.choosePerm(c) }, true
	case top == "trust":
		spans, ok := item(len(trustAnswers))
		if !ok || (a.program != nil && a.now().Sub(a.trust.opened) < permGrace) {
			return nil, nil, false
		}
		answer := trustAnswers[hit-1]
		return spans, func() { a.answerTrust(answer) }, true
	case top == "question":
		return nil, nil, false
	case top == "memReview":
		mr := a.memReview
		return list(len(mr.items), &mr.sel, nil)
	case top == "overlay":
		st := &a.overlays[len(a.overlays)-1]
		var open func()
		if len(st.visible) > 0 && st.enterVerb(st.visible[st.selected]) != "" {
			open = func() { a.overlayKey("enter", "") }
		}
		return list(len(st.visible), &st.selected, open)
	case y >= a.popRect.Min.Y && y < a.popRect.Max.Y && a.completion.open && len(a.completion.matches) > 0:
		f := &a.completion
		spans, ok := item(len(f.matches))
		return spans, func() { f.selected = hit - 1; a.acceptCompletion() }, ok
	case y >= a.popRect.Min.Y && y < a.popRect.Max.Y && a.palette.open:
		p := &a.palette
		spans, ok := item(p.count())
		return spans, func() { p.selected = hit - 1; a.acceptPalette(); a.updatePalette() }, ok
	case under.Click != "":
		act := func() { a.clickWidget(under, x, y) }
		if under.Widget == "" { // a Go statusline pill names its command
			act = func() {
				if under.Click != "reload" {
					a.runCommand(under.Click, "")
				} else if a.reload != nil {
					go a.reload() // off the UI goroutine, as the typed /reload
				}
			}
		} else if w := a.host.Widgets.Get(under.Widget); w == nil || w.OnClick == nil {
			break
		}
		spans, ok := at(func(s hostapi.Segment) bool { return s.Click == under.Click && s.Widget == under.Widget })
		return spans, act, ok
	}
	return nil, nil, false
}

// itemSpans is the cells of the item same matches on the contiguous rows
// around y that carry it, one span per run of text. Blank ends of a segment are dropped
// unless painted (a themed background such as a code span).
// NOTE: only the top chrome box takes input, so contiguity scopes an item;
// two boxes stacked edge to edge with the same ids would merge (bound by the
// box's columns if that ever happens).
func (a *App) itemSpans(y int, same func(hostapi.Segment) bool) []cellSpan {
	carries := func(y int) bool {
		return y >= 0 && y < len(a.lastRows) && slices.ContainsFunc(a.lastRows[y], same)
	}
	for carries(y - 1) {
		y--
	}
	var spans []cellSpan
	for ; carries(y); y++ {
		col, lo, hi := 0, -1, 0
		for _, s := range a.lastRows[y] {
			w := ansi.StringWidth(s.Text)
			if same(s) {
				from, to := col, col+w
				if a.hlStyle(s.HL).Bg == nil {
					from += w - ansi.StringWidth(strings.TrimLeft(s.Text, " "))
					to -= w - ansi.StringWidth(strings.TrimRight(s.Text, " "))
				}
				if from < to {
					// a gap wider than a word space (label to key column) is not the item
					if lo >= 0 && from > hi+1 {
						spans = append(spans, cellSpan{y, lo, hi})
						lo = -1
					}
					if lo < 0 {
						lo = from
					}
					hi = to
				}
			}
			col += w
		}
		if lo >= 0 {
			spans = append(spans, cellSpan{y, lo, hi})
		}
	}
	return spans
}

func plainSegs(segs []hostapi.Segment) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.Text)
	}
	return b.String()
}

// clickWidget runs the on_click of the widget that drew s on the background
// worker, like a keymap.
func (a *App) clickWidget(s hostapi.Segment, x, y int) {
	p := a.press
	ev := map[string]any{
		"id": s.Click, "x": x, "y": y, "clicks": p.clicks,
		"button": map[tea.MouseButton]string{tea.MouseLeft: "left", tea.MouseRight: "right", tea.MouseMiddle: "middle"}[p.button],
		"mods":   map[string]any{"shift": p.mod.Contains(tea.ModShift), "ctrl": p.mod.Contains(tea.ModCtrl), "alt": p.mod.Contains(tea.ModAlt)},
	}
	a.background(func() {
		err := a.host.ClickWidget(context.Background(), s.Widget, ev)
		a.Refresh()
		switch {
		case err == nil, errors.Is(err, lua.ErrHot), errors.Is(err, lua.ErrReloaded):
		default:
			a.host.RecordError("click on widget "+s.Widget, err.Error())
			a.Notify("error", "Click on "+s.Widget+" failed", err.Error())
		}
	})
}

// Copy implements hostapi.UIHooks (/copy).
func (a *App) Copy(text string) {
	a.dispatch(uiFunc(func() tea.Cmd {
		a.pushToast("info", "Copied to the clipboard", "")
		return copyCmd(text)
	}))
}

// copyCmd sets the clipboard through the terminal (works over ssh) and the
// local clipboard tool (for terminals without OSC 52).
func copyCmd(text string) tea.Cmd {
	return tea.Batch(tea.SetClipboard(text), func() tea.Msg {
		for _, c := range [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}} {
			if !has(c[0]) {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			cmd := proc.CommandContext(ctx, c[0], c[1:]...)
			cmd.Stdin = strings.NewReader(text)
			// cmd.Run, not proc.Run: wl-copy and xclip leave a child serving
			// the selection, and proc.Wait kills the group
			err := cmd.Run()
			cancel()
			if err == nil {
				break
			}
		}
		return nil
	})
}
