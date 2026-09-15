// mouse.go: a drag selects screen text and copies it on release (OSC 52
// plus the native clipboard tool); a click without a drag acts on what is
// under it: prompt buttons, list rows, a mode name, the thought line.
// Hovering lights what a click would act on.
package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/proc"
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
		a.pushToast("info", "", "copied to clipboard")
		return copyCmd(text)
	}
	return nil
}

// selectionText is the plain text under the selection, one line per row.
func (a *App) selectionText() string {
	var lines []string
	for y, row := range a.lastRows {
		if from, to, ok := a.mouse.span(y, a.width); ok {
			lines = append(lines, strings.TrimRight(plainSegs(truncateSegs(dropCells(row, from), to-from)), " "))
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
		mid[i].HL = hl(mid[i].HL)
	}
	out := append(append([]hostapi.Segment{}, truncateSegs(row, from)...), mid...)
	return append(out, dropCells(row, to)...)
}

// highlightHover lights what a click under the pointer would act on.
func (a *App) highlightHover(rows [][]hostapi.Segment) {
	if !a.hovering || a.mouse.pressed || a.mouse.dragged {
		return
	}
	if from, to, _, ok := a.target(a.hoverX, a.hoverY); ok {
		rows[a.hoverY] = restyle(rows[a.hoverY], from, to, func(hl string) string {
			switch hl {
			case "Host:BtnSel", "Host:BtnNo":
				return hl
			case "Host:Btn":
				return "Host:BtnHover"
			}
			return "Host:Hover"
		})
	}
}

// hover tracks the pointer; the frame redraws only when what it points at
// changes, and supporting terminals show a hand over clickable things.
func (a *App) hover(x, y int) tea.Cmd {
	a.hovering, a.hoverX, a.hoverY = true, x, y
	from, to, _, ok := a.target(x, y)
	key := ""
	if ok {
		key = fmt.Sprint(y, from, to)
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
	if _, _, act, ok := a.target(x, y); ok {
		act()
		a.publishComposer()
	}
	a.frameGen++
}

// target finds what a click at (x, y) would do: the cells [from, to) of
// row y that act, and the action. Clicks and hover share it, so a
// highlight always means a click does something. A list row is selected by
// the first click and opened by a click on the selected row.
func (a *App) target(x, y int) (from, to int, act func(), ok bool) {
	if y < 0 || y >= len(a.lastRows) {
		return 0, 0, nil, false
	}
	row := a.lastRows[y]
	hit := 0
	if rest := dropCells(row, x); len(rest) > 0 {
		hit = rest[0].Hit
	}
	hitSpan := func() (int, int) {
		col, lo, hi := 0, -1, 0
		for _, s := range row {
			w := ansi.StringWidth(s.Text)
			if s.Hit == hit {
				if lo < 0 {
					lo = col
				}
				hi = col + w
			}
			col += w
		}
		return lo, hi
	}
	// list selects the row on the first click and opens it on the second
	list := func(n int, sel *int, open func()) (int, int, func(), bool) {
		if hit <= 0 || hit > n {
			return 0, 0, nil, false
		}
		lo, hi := hitSpan()
		return lo, hi, func() {
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
		if hit <= 0 || hit > len(choices) || (a.program != nil && a.now().Sub(pa.opened) < permGrace) {
			return 0, 0, nil, false
		}
		lo, hi := hitSpan()
		c := choices[hit-1]
		return lo, hi, func() { a.answerPerm(c.ok, c.by) }, true
	case top == "trust", top == "question", top == "cfgReview":
		return 0, 0, nil, false
	case top == "memReview":
		mr := a.memReview
		return list(len(mr.items), &mr.sel, func() {})
	case top == "modelPick":
		mp := a.modelPick
		return list(len(mp.visible), &mp.selected, func() { a.answerPick(mp.items[mp.visible[mp.selected]].ID, true) })
	case top == "overlay":
		st := &a.overlays[len(a.overlays)-1]
		return list(len(st.visible), &st.selected, func() { a.overlayKey("enter", "") })
	case y < a.lastChatBottom && a.completion.open && len(a.completion.matches) > 0:
		f := &a.completion
		lo, hi, _, ok := list(len(f.matches), &f.selected, nil)
		return lo, hi, func() { f.selected = hit - 1; a.acceptCompletion() }, ok
	case y < a.lastChatBottom && a.palette.open:
		p := &a.palette
		lo, hi, _, ok := list(p.count(), &p.selected, nil)
		return lo, hi, func() { p.selected = hit - 1; a.acceptPalette(); a.updatePalette() }, ok
	case y >= a.lastChatBottom:
		// the statusline mode pill opens the picker; a mode in the strip
		// shown after a switch selects it
		text := plainSegs(row)
		lo, hi := wordSpan(text, x)
		word := ""
		if lo < hi {
			word = ansi.Cut(text, lo, hi)
		}
		for _, def := range a.host.PermModeList() {
			m := def.Name
			if word != m {
				continue
			}
			return lo, hi, func() {
				var err error
				if y == len(a.lastRows)-1 {
					err = a.host.OpenModePicker()
				} else {
					err = a.host.SetPermMode(m)
				}
				if err != nil {
					a.pushToast("error", "mode", err.Error())
				}
			}, true
		}
	default:
		text := plainSegs(row)
		if i := strings.Index(text, "✻ Th"); i >= 0 && (strings.Contains(text, "✻ Thought") || strings.Contains(text, "✻ Thinking")) {
			return ansi.StringWidth(text[:i]), ansi.StringWidth(strings.TrimRight(text, " ")), a.host.ToggleReasoning, true
		}
		// an agent tool row ("● Agent <description> · 4s") opens that run's transcript
		for _, mark := range []string{"● Agent ", "◦ Agent "} {
			i := strings.Index(text, mark)
			if i < 0 || strings.TrimSpace(text[:i]) != "" {
				continue
			}
			desc, _, _ := strings.Cut(strings.TrimRight(text[i+len(mark):], " "), " · ")
			if id := a.host.AgentRunNamed(strings.TrimSuffix(desc, "…")); id != "" {
				return ansi.StringWidth(text[:i]), ansi.StringWidth(strings.TrimRight(text, " ")), func() { _ = a.host.ViewAgent(id) }, true
			}
		}
	}
	return 0, 0, nil, false
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
			err := cmd.Run()
			cancel()
			if err == nil {
				break
			}
		}
		return nil
	})
}
