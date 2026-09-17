// mouse.go: a drag selects screen text and copies it on release (OSC 52
// plus the native clipboard tool); a click without a drag acts on what is
// under it: prompt choices, list rows, a mode name, the thought line.
// Hovering lights what a click would act on.
package ui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/win"
)

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
		if m.Button != tea.MouseLeft {
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
	if key == a.hoverKey {
		return nil
	}
	a.hoverKey = key
	a.frameGen++
	if ok == a.pointerHand {
		return nil
	}
	a.pointerHand = ok
	return tea.Raw(pointerShape(ok))
}

// pointerShape is OSC 22; terminals without it ignore the sequence.
func pointerShape(hand bool) string {
	if hand {
		return "\x1b]22;pointer\x07"
	}
	return "\x1b]22;default\x07"
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
	hit := 0
	if rest := dropCells(row, x); len(rest) > 0 {
		hit = rest[0].Hit
	}
	item := func(n int) ([]cellSpan, bool) {
		if hit <= 0 || hit > n {
			return nil, false
		}
		spans := a.itemSpans(y, hit)
		return spans, slices.ContainsFunc(spans, func(s cellSpan) bool { return s.y == y && s.lo <= x && x < s.hi })
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
	// line is a one-row target over the cells [lo, hi) of row y
	line := func(lo, hi int, act func()) ([]cellSpan, func(), bool) {
		return []cellSpan{{y, lo, hi}}, act, lo <= x && x < hi
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
	case top == "modelPick":
		mp := a.modelPick
		return list(len(mp.visible), &mp.selected, func() { a.answerPick(mp.items[mp.visible[mp.selected]].ID, true) })
	case top == "overlay":
		st := &a.overlays[len(a.overlays)-1]
		var open func()
		if len(st.visible) > 0 && st.enterVerb(st.visible[st.selected]) != "" {
			open = func() { a.overlayKey("enter", "") }
		}
		return list(len(st.visible), &st.selected, open)
	case y < a.lastChatBottom && a.completion.open && len(a.completion.matches) > 0:
		f := &a.completion
		spans, ok := item(len(f.matches))
		return spans, func() { f.selected = hit - 1; a.acceptCompletion() }, ok
	case y < a.lastChatBottom && a.palette.open:
		p := &a.palette
		spans, ok := item(p.count())
		return spans, func() { p.selected = hit - 1; a.acceptPalette(); a.updatePalette() }, ok
	case y >= a.lastChatBottom:
		// the statusline mode pill opens the picker
		text := plainSegs(row)
		lo, hi := wordSpan(text, x)
		if y != len(a.lastRows)-1 || lo >= hi {
			break
		}
		word := ansi.Cut(text, lo, hi)
		for _, def := range a.host.PermModeList() {
			if word == def.Name {
				return line(lo, hi, func() {
					if err := a.host.OpenModePicker(); err != nil {
						a.pushToast("error", "Mode picker failed", err.Error())
					}
				})
			}
		}
	default:
		text := plainSegs(row)
		end := ansi.StringWidth(strings.TrimRight(text, " "))
		if before, _, ok := strings.Cut(text, "✻ Th"); ok && (strings.Contains(text, "✻ Thought") || strings.Contains(text, "✻ Thinking")) {
			return line(ansi.StringWidth(before), end, a.host.ToggleReasoning)
		}
		// an agent tool row ("● Agent <description> · 4s") opens that run's transcript
		for _, mark := range []string{"● Agent ", "◦ Agent "} {
			i := strings.Index(text, mark)
			if i < 0 || strings.TrimSpace(text[:i]) != "" {
				continue
			}
			desc, _, _ := strings.Cut(strings.TrimRight(text[i+len(mark):], " "), " · ")
			if id := a.host.AgentRunNamed(strings.TrimSuffix(desc, "…")); id != "" {
				return line(ansi.StringWidth(text[:i]), end, func() { _ = a.host.ViewAgent(id) })
			}
		}
	}
	return nil, nil, false
}

// itemSpans is the cells of item hit on the contiguous rows around y that
// carry it, one span per run of text. Blank ends of a segment are dropped
// unless painted (a themed background such as a code span).
// NOTE: only the top chrome box takes input, so contiguity scopes an item;
// two boxes stacked edge to edge with the same ids would merge (bound by the
// box's columns if that ever happens).
func (a *App) itemSpans(y, hit int) []cellSpan {
	carries := func(y int) bool {
		return y >= 0 && y < len(a.lastRows) && slices.ContainsFunc(a.lastRows[y], func(s hostapi.Segment) bool { return s.Hit == hit })
	}
	for carries(y - 1) {
		y--
	}
	var spans []cellSpan
	for ; carries(y); y++ {
		col, lo, hi := 0, -1, 0
		for _, s := range a.lastRows[y] {
			w := ansi.StringWidth(s.Text)
			if s.Hit == hit {
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

// wordSpan returns the cells [from, to) of the space-delimited word
// covering display cell x (from == to when there is none).
func wordSpan(text string, x int) (from, to int) {
	type cell struct {
		col, w int
		space  bool
	}
	var cells []cell
	col, at := 0, -1
	for _, r := range text {
		w := ansi.StringWidth(string(r))
		if col <= x && x < col+max(w, 1) {
			at = len(cells)
		}
		cells = append(cells, cell{col, w, r == ' '})
		col += w
	}
	if at < 0 || cells[at].space {
		return 0, 0
	}
	i, j := at, at
	for i > 0 && !cells[i-1].space {
		i--
	}
	for j+1 < len(cells) && !cells[j+1].space {
		j++
	}
	return cells[i].col, cells[j].col + cells[j].w
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
