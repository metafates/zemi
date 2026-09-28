// mouse.go: a drag selects screen text and copies it on release (OSC 52
// plus the native clipboard tool); a click without a drag acts on what is
// under it: prompt choices, list rows, widget click ids, the thought line.
// Hovering lights what a click would act on.
package ui

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
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

func (a *App) mouseEnabled() bool { return a.host.Settings.Bool("ui.mouse") }

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
		a.mouse = mouseSel{pressed: true, x0: m.X, y0: m.Y, x1: m.X, y1: m.Y}
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
	if _, _, _, box := a.boxScroll(); !box {
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
			a.scrollRows(b.region, rel*(st.rows-st.viewH)/span-max(st.top, 0))
		}
		return
	}
	switch text, sel, n, _ := a.boxScroll(); {
	case text != nil && text.overflows():
		text.scroll(rel*(text.rows-text.viewH)/span - max(text.top, 0))
	case sel != nil:
		*sel = rel * (n - 1) / span
	}
	a.bump()
}

// dismissOutside closes a menu or popup a click landed outside of, and
// reports whether it did. A blocking ask (permission, trust, question,
// memory review) stays: a tool waits on its answer, so a stray click must
// not decide it.
func (a *App) dismissOutside(x, y int) bool {
	out := func(r uv.Rectangle) bool { return !r.Empty() && !(uv.Position{X: x, Y: y}).In(r) }
	switch {
	case a.topChrome() == modalOverlay && out(a.chromeRect):
		// the whole menu goes, not only a detail view drawn over it
		i := len(a.overlays) - 1
		for i > 0 && a.overlays[i].back {
			i--
		}
		a.cancelOverlay(a.overlays[i].o)
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
					s.HL, s.Over = "Host:Selection", ""
				}
			})
		}
	}
}

// restyle changes the style of cells [from, to) of a row.
func restyle(row []hostapi.Segment, from, to int, style func(*hostapi.Segment)) []hostapi.Segment {
	mid := truncateSegs(dropCells(row, from), to-from)
	mid = append([]hostapi.Segment(nil), mid...)
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
		style = func(s *hostapi.Segment) { s.HL = "Host:Hover" }
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
	key := ""
	if ok {
		key = fmt.Sprint(spans)
	}
	if key != a.hoverKey {
		a.hoverKey = key
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
	pinID, pinSpan := a.pinAt(x, y, under)
	switch top := a.topChrome(); {
	case top == modalPerm:
		pa := a.permAsk
		choices := pa.choices()
		spans, ok := item(len(choices))
		if !ok || (a.program != nil && a.now().Sub(pa.opened) < permGrace) {
			return nil, nil, false
		}
		c := choices[hit-1]
		return spans, func() { a.choosePerm(c) }, true
	case top == modalTrust:
		spans, ok := item(len(trustAnswers))
		if !ok || (a.program != nil && a.now().Sub(a.trust.opened) < permGrace) {
			return nil, nil, false
		}
		answer := trustAnswers[hit-1]
		return spans, func() { a.answerTrust(answer) }, true
	case top == modalPlan:
		spans, ok := item(2) // the buttons; the changes input has no hits
		key := map[int]string{1: "a", 2: "e"}[hit]
		return spans, func() { a.planKey(key, "") }, ok
	case top == modalQuestion:
		qp := a.question
		if !qp.reviewing() && qp.states[qp.at].typing {
			return nil, nil, false // the row is an input, not a choice
		}
		sel, n := qp.sel()
		if !qp.reviewing() && !qp.qs[qp.at].Multi() { // buttons: a click answers
			spans, ok := item(n)
			return spans, func() { *sel = hit - 1; a.questionKey("enter", "") }, ok
		}
		return list(n, sel, func() { a.questionKey("enter", "") })
	case top == modalMemReview:
		mr := a.memReview
		if mr.detail {
			acts := memActions(mr.items[mr.at()], len(mr.undo) > 0)
			spans, ok := item(len(acts))
			return spans, func() { a.memAct(acts[hit-1].key) }, ok
		}
		return list(len(mr.visible), &mr.selected, mr.open)
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
		// Click with no widget runs a command
		return []cellSpan{pinSpan}, func() {
			if st := a.scrolls[pinID]; st != nil {
				a.scrollRows(pinID, st.pin-st.top)
			}
		}, true
	case top == noModal && under.Click != "":
		act := func() { a.clickWidget(under, x, y) }
		if under.Widget == "" { // a Go pill or dock entry names its command
			name, args := hostapi.SplitCommand(under.Click)
			act = func() {
				if name != "reload" {
					a.runCommand(name, args)
				} else if a.reload != nil {
					go a.reload() // off the UI goroutine, as the typed /reload
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

// pinAt is the scrolled region whose pinned row's text holds (x, y), and
// those cells; a zero span when there is none. The cell under the pointer
// must be the pin's own (under), not a toast's drawn over it; a box placed
// over the pin cuts its segment, so under may be a prefix of the pin's.
func (a *App) pinAt(x, y int, under hostapi.Segment) (string, cellSpan) {
	for _, id := range a.scrollIDs {
		st := a.scrolls[id]
		if st == nil || st.first == nil || st.rect.Min.Y != y {
			continue
		}
		if own := dropCells(st.first, x-st.rect.Min.X); len(own) == 0 || own[0].HL != under.HL || !strings.HasPrefix(own[0].Text, under.Text) {
			continue
		}
		text := strings.TrimRight(plainSegs(st.first), " ")
		if sp := (cellSpan{y, st.rect.Min.X, st.rect.Min.X + ansi.StringWidth(text)}); sp.lo <= x && x < sp.hi {
			return id, sp
		}
	}
	return "", cellSpan{}
}

// itemSpans is the cells of the item same matches on the contiguous rows
// around y that carry it, one span per run of text. Blank ends of a segment are dropped
// unless painted (a themed background such as a code span).
// NOTE: only the top chrome box takes input, so contiguity scopes an item;
// two boxes stacked edge to edge with the same ids would merge (bound by the
// box's columns if that ever happens).
// painted is true of a segment filled with something other than the app's
// own canvas: its cells are content, not backdrop, so a list's label/key
// spacers (which ride the item's id) never widen the target.
func (a *App) painted(s hostapi.Segment) bool {
	bg := a.hlStyle(s.HL).Bg
	if bg == nil {
		return false
	}
	if canvas := appBG(a.thm, a.thm.active()); canvas != nil {
		return !sameColor(bg, canvas)
	}
	return true
}

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
		case err == nil, lua.Dropped(err):
		default:
			a.host.RecordError("click "+s.Widget, err.Error())
			a.Notify(logging.Error, "Could not handle the click on "+s.Widget, err.Error())
		}
	})
}

// copied copies text and says how much it was.
func (a *App) copied(text string) tea.Cmd {
	size := fmt.Sprintf("%d chars", utf8.RuneCountInString(text))
	if n := strings.Count(text, "\n") + 1; n > 1 {
		size = fmt.Sprintf("%d lines, %s", n, size)
	}
	a.pushToast(logging.Info, "Copied", size)
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
