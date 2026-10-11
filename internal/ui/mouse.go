// mouse.go: a drag selects screen text and copies it on release (OSC 52
// plus the native clipboard tool); a click without a drag acts on what is
// under it: prompt choices, list rows, widget click ids, the thought line.
// Hovering lights what a click would act on.

package ui

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/proc"
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
	// the text cells of the box or pane the press landed in: a selection
	// never takes another pane's text
	clip uv.Rectangle
}

// span is the selected cell range [from, to) of screen row y. Both ends are
// held inside clip, so a drag past its edge selects to the edge.
func (s mouseSel) span(y, w int) (from, to int, ok bool) {
	if !s.dragged || s.clip.Empty() {
		return 0, 0, false
	}
	ax, ay := clamp(s.clip, s.x0, s.y0)
	bx, by := clamp(s.clip, s.x1, s.y1)
	if by < ay || (by == ay && bx < ax) {
		ay, ax, by, bx = by, bx, ay, ax
	}
	if y < ay || y > by {
		return 0, 0, false
	}
	from, to = s.clip.Min.X, min(s.clip.Max.X, w)
	if y == ay {
		from = ax
	}
	if y == by {
		to = bx + 1
	}
	return from, to, to > from
}

// clamp holds (x, y) inside r: a point below it goes to its last cell, one
// above to its first, else x is held in its columns.
func clamp(r uv.Rectangle, x, y int) (int, int) {
	switch {
	case y >= r.Max.Y:
		return r.Max.X - 1, r.Max.Y - 1
	case y < r.Min.Y:
		return r.Min.X, r.Min.Y
	}
	return max(r.Min.X, min(x, r.Max.X-1)), y
}

// selClip is where a drag pressed at (x, y) may select: the top modal's
// text, a composer popup's, else the layout pane under it; nothing outside
// a modal, whose backdrop is dimmed, or on a separator or spacer. A toast's
// cells are NoCopy, so a drag under one takes the box's text alone.
func (a *App) selClip(x, y int) uv.Rectangle {
	p := uv.Position{X: x, Y: y}
	switch {
	case a.topChrome() != noModal:
		if p.In(a.chromeRect) {
			return boxText(a.chromeRect)
		}
		return uv.Rectangle{}
	case p.In(a.popRect):
		return boxText(a.popRect)
	}
	for _, r := range slices.Backward(a.panes) {
		if p.In(r) {
			return r
		}
	}
	return uv.Rectangle{}
}

// boxText is the text cells of a promptBox at r: inside its border and the
// pad beside it.
func boxText(r uv.Rectangle) uv.Rectangle {
	return uv.Rect(r.Min.X+2, r.Min.Y+1, max(r.Dx()-4, 0), max(r.Dy()-2, 0))
}

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
		if a.barDrag = a.barAt(m.X, m.Y); a.barDrag.h > 0 {
			a.dragBar(m.Y)
			return nil
		}
		a.mouse = mouseSel{pressed: true, x0: m.X, y0: m.Y, x1: m.X, y1: m.Y, clip: a.selClip(m.X, m.Y)}
		a.bump()
	case tea.MouseMotionMsg:
		if a.barDrag.h > 0 {
			a.dragBar(m.Y)
			return nil
		}
		if !a.mouse.pressed {
			return a.hover(m.X, m.Y)
		}
		if m.X != a.mouse.x1 || m.Y != a.mouse.y1 {
			a.mouse.x1, a.mouse.y1, a.mouse.dragged = m.X, m.Y, true
			a.bump()
		}
	case tea.MouseReleaseMsg:
		if a.barDrag.h > 0 {
			a.barDrag = barHold{}
			return nil
		}
		if !a.mouse.pressed {
			return nil
		}
		a.mouse.pressed = false
		a.bump()
		if !a.mouse.dragged {
			a.mouse = mouseSel{}
			if !a.dismissOutside(m.X, m.Y) {
				a.focusAt(m.X, m.Y)
				a.click(m.X, m.Y)
			}
			return a.hover(m.X, m.Y)
		}
		text := a.selectionText()
		if text == "" {
			a.mouse = mouseSel{}
			return nil
		}
		return a.copied(text)
	}
	return nil
}

// barHold is a scrollbar the pointer holds: the screen rows of its column,
// and the scrolled region it scrolls, "" for the box on top.
type barHold struct {
	region string
	top, h int
}

// barAt is the scrollbar drawn at (x, y), h 0 for none. It reads the last
// frame, where only a live bar is a Bar cell: dimmed chrome drops it.
func (a *App) barAt(x, y int) barHold {
	bar := func(y int) bool {
		if y < 0 || y >= len(a.lastRows) {
			return false
		}
		rest := dropCells(a.lastRows[y], x)
		return len(rest) > 0 && rest[0].Bar
	}
	if !bar(y) {
		return barHold{}
	}
	b := barHold{top: y, h: 1}
	for bar(b.top - 1) {
		b.top, b.h = b.top-1, b.h+1
	}
	for bar(b.top + b.h) {
		b.h++
	}
	// a bar is the box's only inside it: a popup dims nothing, so the bars of
	// the regions beside it stay live
	if p := (uv.Position{X: x, Y: y}); !p.In(a.chromeRect) && !p.In(a.popRect) {
		b.region = a.scrollAt(x, y)
	}
	return b
}

// dragBar puts the held bar's viewport where the pointer is along the bar:
// a text's top row, or a list's selection.
func (a *App) dragBar(y int) {
	b := a.barDrag
	rel, span := max(0, min(y-b.top, b.h-1)), max(b.h-1, 1)
	if b.region != "" {
		if st := a.scrolls[b.region]; st != nil {
			a.scrollRows(b.region, rel*(st.rows-st.viewH)/span-st.top)
		}
		return
	}
	switch text, sel, n, _ := a.boxScroll(); {
	case text != nil && text.overflows():
		text.scroll(rel*(text.rows-text.viewH)/span - text.top)
	case sel != nil:
		*sel = rel * (n - 1) / span
	}
	a.bump()
}

// dismissOutside closes a menu or popup a click landed outside of, and
// reports whether it did. A blocking ask (permission, trust, question,
// plan) stays: a tool waits on its answer, so a stray click must not decide
// it.
func (a *App) dismissOutside(x, y int) bool {
	out := func(r uv.Rectangle) bool { return !r.Empty() && !(uv.Position{X: x, Y: y}).In(r) }
	switch {
	case a.topChrome() == modalOverlay && out(a.chromeRect):
		a.cancelMenu()
	case a.topChrome() != noModal:
		return false
	case a.palette.open && out(a.popRect):
		a.palette.close()
	case a.completion.shown() && out(a.popRect):
		a.completion.close()
	default:
		return false
	}
	a.bump()
	return true
}

// focusAt gives the keys to the focusable node a click lands in (the topmost:
// floats come last), or to the composer when it lands there.
func (a *App) focusAt(x, y int) {
	if a.topChrome() != noModal {
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

// selectionText is the plain text under the selection, one line per row:
// decoration (NoCopy) is left out, and a row that soft-wraps joins the next.
func (a *App) selectionText() string {
	var b strings.Builder
	joined := true // no newline before the first line
	for y, row := range a.lastRows {
		from, to, ok := a.mouse.span(y, a.width)
		if !ok {
			continue
		}
		var line strings.Builder
		soft := false
		for _, s := range truncateSegs(dropCells(row, from), to-from) {
			if !s.NoCopy {
				line.WriteString(s.Text)
			}
			if soft = s.Soft; soft {
				break // what follows on the row is padding or another column
			}
		}
		text := line.String()
		if !soft {
			text = strings.TrimRight(text, " ")
		}
		if !joined {
			b.WriteByte('\n')
		}
		b.WriteString(text)
		joined = soft
	}
	return strings.TrimRight(strings.Trim(b.String(), "\n"), " ")
}

// highlightSelection restyles the selected cells of the composed rows,
// decoration aside.
func (a *App) highlightSelection(rows [][]hostapi.Segment, w int) {
	for y := range rows {
		if from, to, ok := a.mouse.span(y, w); ok {
			rows[y] = restyle(rows[y], from, to, func(s *hostapi.Segment) {
				if !s.NoCopy {
					s.HL, s.Over = hostapi.ChromeSelection, ""
				}
			})
		}
	}
}

// restyle changes the style of cells [from, to) of a row.
func restyle(row []hostapi.Segment, from, to int, style func(*hostapi.Segment)) []hostapi.Segment {
	mid := slices.Clone(truncateSegs(dropCells(row, from), to-from))
	for i := range mid {
		if !strings.ContainsRune(mid[i].Text, kitty.Placeholder) { // its color names the image
			style(&mid[i])
		}
	}
	out := append(append([]hostapi.Segment{}, truncateSegs(row, from)...), mid...)
	return append(out, dropCells(row, to)...)
}

// highlightHover lights what a click under the pointer would act on: the
// theme's HoverItem over a list row, its Hover over inline targets, and a
// fixed style no theme reaches over a Go prompt's choices (DESIGN §8).
func (a *App) highlightHover(rows [][]hostapi.Segment) {
	if !a.hovering || a.mouse.pressed || a.mouse.dragged {
		return
	}
	spans, _, ok := a.target(a.hoverX, a.hoverY)
	if !ok {
		return
	}
	style := func(s *hostapi.Segment) { s.Over = "Hover" }
	if top := a.topChrome(); top == modalPerm || top == modalTrust {
		style = func(s *hostapi.Segment) { s.HL = hostapi.ChromeHover }
	} else if rest := dropCells(rows[a.hoverY], a.hoverX); len(rest) > 0 && rest[0].Hit > 0 {
		style = func(s *hostapi.Segment) { s.Over = "HoverItem" }
	}
	for _, sp := range spans {
		rows[sp.y] = restyle(rows[sp.y], sp.lo, sp.hi, func(s *hostapi.Segment) {
			if !s.NoHover { // a body row says it acts with the pointer alone
				style(s)
			}
		})
	}
}

// hover tracks the pointer; the frame redraws only when what it points at
// changes, and supporting terminals show a hand over clickable things.
func (a *App) hover(x, y int) tea.Cmd {
	a.hovering, a.hoverX, a.hoverY = true, x, y
	spans, _, ok := a.target(x, y)
	var cur []cellSpan
	if ok {
		cur = spans
	}
	if !slices.Equal(cur, a.hoverSpans) {
		a.hoverSpans = cur
		a.bump()
	}
	shape := ""
	if ok {
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
	return tea.Raw(ansi.SetPointerShape(shape))
}

// click acts on the topmost thing under (x, y).
func (a *App) click(x, y int) {
	if _, act, ok := a.target(x, y); ok {
		act()
		a.publishComposer()
	}
	a.bump()
}

// cellSpan is the cells [lo, hi) of screen row y.
type cellSpan struct{ y, lo, hi int }

// firstRow keeps the spans of the topmost row only.
func firstRow(spans []cellSpan) []cellSpan {
	if len(spans) == 0 {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(spans), func(s cellSpan) bool { return s.y != spans[0].y })
}

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
	pinID, pinK, pinSpan := a.pinAt(x, y, under)
	switch top := a.topChrome(); {
	case top == modalPerm:
		pa := a.permAsk
		choices := pa.choices()
		spans, ok := item(len(choices))
		if !ok || a.fresh(pa.opened) {
			return nil, nil, false
		}
		c := choices[hit-1]
		return spans, func() { a.choosePerm(c) }, true
	case top == modalTrust:
		spans, ok := item(len(trustAnswers))
		if !ok || a.fresh(a.trust.opened) {
			return nil, nil, false
		}
		answer := trustAnswers[hit-1]
		return spans, func() { a.answerTrust(answer) }, true
	case top == modalPlan:
		p := a.planAsk
		choices := p.choices()
		spans, ok := item(len(choices)) // the buttons; the changes input has no hits
		if !ok {
			return spans, nil, false
		}
		c := choices[hit-1]
		return spans, func() { a.planKey(c.key, "") }, true
	case top == modalQuestion:
		qp := a.question
		if qp.reviewing() { // a summary row opens its question, the button submits
			n := len(qp.qs)
			spans, ok := item(n + 1)
			return spans, func() {
				if hit > n {
					a.finishQuestion(true)
				} else {
					qp.at = hit - 1
				}
			}, ok
		}
		q, st := qp.qs[qp.at], &qp.states[qp.at]
		n := q.ListRows()
		switch {
		case st.typing:
			return nil, nil, false // the field is an input, not a choice
		case hit == n+1:
			spans, ok := item(n + 1)
			return spans, func() { st.typing = true }, ok
		case !q.Multi(): // buttons: a click answers
			spans, ok := item(n)
			return spans, func() { st.sel = hit - 1; a.questionKey("enter", "") }, ok
		}
		return list(n, &st.sel, func() { a.questionKey("enter", "") })
	case top == modalOverlay:
		st := &a.overlays[len(a.overlays)-1]
		var open func()
		if len(st.visible) > 0 && st.enterVerb(st.visible[st.selected]) != "" {
			open = func() { a.overlayKey("enter", "") }
		}
		return list(len(st.visible), &st.selected, open)
	case y >= a.popRect.Min.Y && y < a.popRect.Max.Y && a.completion.open && a.completion.count() > 0:
		f := &a.completion
		spans, ok := item(f.count())
		return spans, func() { f.selected = hit - 1; a.acceptCompletion() }, ok
	case y >= a.popRect.Min.Y && y < a.popRect.Max.Y && a.palette.open:
		p := &a.palette
		spans, ok := item(p.count())
		return spans, func() { p.selected = hit - 1; a.acceptPalette() }, ok
	case top == noModal && pinSpan.hi > 0:
		// found by where it is: a Hit would light it as a list row, and a
		// Click with no widget runs a command. The header lands on its own
		// row of the pins, under the ones above it.
		return []cellSpan{pinSpan}, func() {
			if st := a.scrolls[pinID]; st != nil && pinK < len(st.pin) {
				a.scrollRows(pinID, st.pin[pinK]-pinK-st.top)
			}
		}, true
	case top == noModal && under.Click != "":
		act := func() { a.clickWidget(under, x, y) }
		if under.Widget == "" { // a Go pill or dock entry names its command
			name, args := hostapi.SplitCommand(under.Click)
			act = func() {
				if name != "reload" {
					a.runCommand(name, args)
				} else {
					a.host.GoSafe("reload", a.host.Reload) // off the UI goroutine, as the typed /reload
				}
			}
		} else if w := a.host.Widgets.Get(under.Widget); w == nil || w.OnClick == nil {
			break
		}
		spans, ok := at(func(s hostapi.Segment) bool { return s.Click == under.Click && s.Widget == under.Widget })
		// a click acts anywhere on the target, but only its first row lights:
		// underlining a whole diff says nothing about what the click does
		return firstRow(spans), act, ok
	}
	return nil, nil, false
}

// pinAt is the scrolled region whose pinned row's text holds (x, y), which
// of its pins that is, and those cells; a zero span when there is none. The
// cell under the pointer must be the pin's own (under), not a toast's drawn
// over it; a box placed over the pin cuts its segment, so under may be a
// prefix of the pin's.
func (a *App) pinAt(x, y int, under hostapi.Segment) (string, int, cellSpan) {
	for _, id := range a.scrollIDs {
		st := a.scrolls[id]
		if st == nil {
			continue
		}
		k := y - st.rect.Min.Y
		if k < 0 || k >= len(st.pins) {
			continue
		}
		pin := st.pins[k]
		if own := dropCells(pin, x-st.rect.Min.X); len(own) == 0 || own[0].HL != under.HL || !strings.HasPrefix(own[0].Text, under.Text) {
			continue
		}
		text := strings.TrimRight(hostapi.SegsText(pin), " ")
		if sp := (cellSpan{y: y, lo: st.rect.Min.X, hi: st.rect.Min.X + ansi.StringWidth(text)}); sp.lo <= x && x < sp.hi {
			return id, k, sp
		}
	}
	return "", 0, cellSpan{}
}

// painted is true of a segment filled with something other than the app's
// canvas: its cells are content, while a list's blank spacers are backdrop.
func (a *App) painted(s hostapi.Segment) bool {
	bg := a.hlStyle(s.HL).Bg
	if bg == nil {
		return false
	}
	if canvas := a.thm.canvas(); canvas != nil {
		return !sameColor(bg, canvas)
	}
	return true
}

// itemSpans is the cells of the item same matches on the contiguous rows
// around y that carry it, one span per run of text; blank ends not painted
// are dropped.
// NOTE: only the top box takes input, so contiguity scopes an item; two boxes
// stacked edge to edge with the same ids would merge.
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
				if !a.painted(s) {
					from += w - ansi.StringWidth(strings.TrimLeft(s.Text, " "))
					to -= w - ansi.StringWidth(strings.TrimRight(s.Text, " "))
				}
				if from < to {
					// a gap wider than a word space (label to key column) is not the item
					if lo >= 0 && from > hi+1 {
						spans = append(spans, cellSpan{y: y, lo: lo, hi: hi})
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
			spans = append(spans, cellSpan{y: y, lo: lo, hi: hi})
		}
	}
	return spans
}

// clickWidget runs the on_click of the widget that drew s on the background
// worker, like a keymap. The click leaves the region it acts in off its
// tail: the rows that widget redraws move under the reader, and a following
// viewport would scroll them away instead of leaving the reader's place.
func (a *App) clickWidget(s hostapi.Segment, x, y int) {
	a.leaveTail(s.Widget)
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
		case err == nil, lua.Dropped(err):
		default:
			a.host.RecordError("click "+s.Widget, err.Error())
			a.Notify(slog.LevelError, "Could not handle the click on "+s.Widget, err.Error())
		}
	})
}

// copied copies text and says how much it was.
func (a *App) copied(text string) tea.Cmd {
	size := fmt.Sprintf("%d chars", utf8.RuneCountInString(text))
	if n := strings.Count(text, "\n") + 1; n > 1 {
		size = fmt.Sprintf("%d lines, %s", n, size)
	}
	a.pushToast(slog.LevelInfo, "Copied", size)
	return copyCmd(text)
}

// Copy implements hostapi.UIHooks (/copy).
func (a *App) Copy(text string) {
	a.dispatch(uiFunc(func() tea.Cmd {
		return a.copied(text)
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
