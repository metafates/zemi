// mouse.go: a drag selects screen text and copies it on release (OSC 52
// plus the native clipboard tool); a click without a drag acts on what is
// under it: prompt buttons, list rows, a mode name, the thought line.
package ui

import (
	"context"
	"os/exec"
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
		if a.mouse.pressed && (m.X != a.mouse.x1 || m.Y != a.mouse.y1) {
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
			return nil
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
	for y, row := range rows {
		from, to, ok := a.mouse.span(y, w)
		if !ok {
			continue
		}
		mid := truncateSegs(dropCells(row, from), to-from)
		for i := range mid {
			mid[i].HL = "Host:Selection"
		}
		out := append(append([]hostapi.Segment{}, truncateSegs(row, from)...), mid...)
		rows[y] = append(out, dropCells(row, to)...)
	}
}

// click acts on the topmost thing under (x, y). A list row is selected by
// the first click and opened by a click on the selected row.
func (a *App) click(x, y int) {
	if y < 0 || y >= len(a.lastRows) {
		return
	}
	row := a.lastRows[y]
	hit := 0
	if rest := dropCells(row, x); len(rest) > 0 {
		hit = rest[0].Hit
	}
	switch {
	case a.permAsk != nil:
		pa := a.permAsk
		if choices := pa.choices(); hit > 0 && hit <= len(choices) && (a.program == nil || a.now().Sub(pa.opened) >= permGrace) {
			a.answerPerm(choices[hit-1].ok, choices[hit-1].by)
		}
	case a.trust != nil, a.question != nil, a.cfgReview != nil:
	case a.memReview != nil:
		if hit > 0 {
			a.memReview.sel = hit - 1
		}
	case a.modelPick != nil:
		if mp := a.modelPick; hit > 0 && hit <= len(mp.visible) {
			if mp.selected == hit-1 {
				a.answerPick(mp.items[mp.visible[hit-1]].ID, true)
			} else {
				mp.selected = hit - 1
			}
		}
	case len(a.overlays) > 0:
		if st := &a.overlays[len(a.overlays)-1]; hit > 0 && hit <= len(st.visible) {
			if st.selected == hit-1 {
				a.overlayKey("enter", "")
			} else {
				st.selected = hit - 1
			}
		}
	case y >= a.lastChatBottom:
		// the statusline mode pill opens the picker; a mode in the strip
		// shown after a switch selects it
		word := wordAt(plainSegs(row), x)
		for _, m := range hostapi.PermModes {
			if word != m {
				continue
			}
			var err error
			if y == len(a.lastRows)-1 {
				err = a.host.OpenModePicker()
			} else {
				err = a.host.SetPermMode(m)
			}
			if err != nil {
				a.pushToast("error", "mode", err.Error())
			}
		}
	default:
		if text := plainSegs(row); strings.Contains(text, "✻ Thought") || strings.Contains(text, "✻ Thinking") {
			a.host.ToggleReasoning()
		}
	}
	a.frameGen++
}

func plainSegs(segs []hostapi.Segment) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.Text)
	}
	return b.String()
}

// wordAt returns the space-delimited word covering display cell x.
func wordAt(text string, x int) string {
	var runes []rune
	col, at := 0, -1
	for _, r := range text {
		if col <= x && x < col+max(ansi.StringWidth(string(r)), 1) {
			at = len(runes)
		}
		col += ansi.StringWidth(string(r))
		runes = append(runes, r)
	}
	if at < 0 || runes[at] == ' ' {
		return ""
	}
	i, j := at, at
	for i > 0 && runes[i-1] != ' ' {
		i--
	}
	for j < len(runes) && runes[j] != ' ' {
		j++
	}
	return string(runes[i:j])
}

// copyCmd sets the clipboard through the terminal (works over ssh) and the
// local clipboard tool (for terminals without OSC 52).
func copyCmd(text string) tea.Cmd {
	return tea.Batch(tea.SetClipboard(text), func() tea.Msg {
		for _, c := range [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}} {
			if _, err := exec.LookPath(c[0]); err != nil {
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
