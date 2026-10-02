// agent.go is the Go-owned agent chrome (DESIGN §6.3, §8): the permission
// prompt, the shimmer effect, and the shared box and list renderers.
// Their chrome uses "Host:*" styles in the palette's roles, which no theme
// group can restyle, and no Lua participates in drawing or answering them.

package ui

import (
	"cmp"
	"encoding/json/jsontext"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
)

// permAskPrompt is the open permission overlay; the host hands the UI one
// ask at a time (Host.AsksWaiting counts it and the ones behind it).
type permAskPrompt struct {
	req hostapi.PermRequest
	// answer is nil once answered: the box stays drawn, inert, while more
	// asks wait, so the next replaces it in place instead of blanking the stage
	answer chan permAnswer
	opened time.Time // keys typed just before it appeared must not answer it
	sel    int       // highlighted choice
	view   viewport  // the command or preview
	size   boxSize
}

// permGrace swallows keys right after a prompt opens: someone typing a
// message should not approve a command by accident.
const permGrace = 400 * time.Millisecond

// fresh reports whether a prompt opened within permGrace.
func (a *App) fresh(opened time.Time) bool {
	return a.program != nil && a.now().Sub(opened) < permGrace
}

// permChoice is one answer of the permission prompt: its button, whose
// label says how long and desc what it allows, and the answer it gives.
type permChoice struct {
	promptChoice
	by   string
	ok   bool
	tell bool // deny and stop the turn so the user can say what to do instead
}

func (pa *permAskPrompt) choices() []permChoice {
	out := []permChoice{{key: "y", label: "Yes", by: "user:once", ok: true}}
	if pa.req.Always != "" {
		out = append(out, permChoice{key: "a", label: "Yes, this session", desc: pa.req.Always, by: "user:always", ok: true})
	}
	if pa.req.Wider != "" {
		out = append(out, permChoice{key: "r", label: "Yes, this session", desc: pa.req.Wider, by: "user:wider", ok: true})
	}
	if pa.req.Project {
		out = append(out, permChoice{key: "p", label: "Yes, this project", desc: "the same, kept after zemi exits", by: "user:project", ok: true})
	}
	return append(out, permChoice{key: "n", label: "No", by: tools.ByUser},
		permChoice{key: "t", label: "No, and tell zemi", desc: "stop the turn and say what to do instead", by: tools.ByUser, tell: true})
}

// choosePerm answers the prompt with c. Telling zemi what to do also denies
// the waiting asks and interrupts the turn; the composer already has focus.
func (a *App) choosePerm(c permChoice) {
	if a.permAsk == nil || a.permAsk.answer == nil {
		return // a click on the last frame's box after it was answered
	}
	if c.tell {
		a.host.DenyWaitingAsks()
	}
	a.answerPerm(c.ok, c.by)
	if c.tell && a.host.AgentActive() {
		a.interrupt()
	}
}

// AgentStream implements hostapi.UIHooks (called from the turn goroutine).
// Stream events only invalidate the frame, so one waits in the queue at a
// time: a token per message would crowd out hooks that carry state.
func (a *App) AgentStream() {
	if a.streamPending.Swap(true) {
		return
	}
	if !a.dispatch(agentStreamMsg{}) {
		a.streamPending.Store(false)
	}
}

// TurnDone implements hostapi.UIHooks: the end of a turn always gets in.
func (a *App) TurnDone(err string, stopped bool) {
	a.dispatch(turnDoneMsg{err: err, stopped: stopped})
}

// AskPermission implements hostapi.UIHooks. It blocks the caller (a tool
// run on the turn goroutine) until the user answers the overlay; safe from
// any goroutine via the hook channel + buffered answer, mirroring AskTrust.
func (a *App) AskPermission(req hostapi.PermRequest) (bool, string) {
	// Every field carries model input: strip escapes before the chrome.
	req.Subject, req.Arg = sanitize.String(req.Subject), sanitize.String(req.Arg)
	req.Reason, req.Preview = sanitize.String(req.Reason), sanitize.String(req.Preview)
	req.Description, req.Always = sanitize.String(req.Description), sanitize.String(req.Always)
	req.Wider, req.Agent, req.Owner = sanitize.String(req.Wider), sanitize.Line(req.Agent), sanitize.Line(req.Owner)
	req.Runs = slices.Clone(req.Runs)
	for i := range req.Runs {
		req.Runs[i].Name = sanitize.Line(req.Runs[i].Name)
	}
	// a tool's input reads as its fields, one per line
	if strings.HasPrefix(req.Preview, "{") {
		if out, err := jsontext.AppendFormat(nil, req.Preview, jsontext.WithIndent("  ")); err == nil {
			req.Preview = string(out)
		}
	}
	ch := make(chan permAnswer, 1)
	a.await(func() tea.Cmd {
		p := &permAskPrompt{req: req, answer: ch, opened: a.now()}
		if a.permAsk != nil { // the answered box: keep its size so nothing jumps
			p.size = a.permAsk.size
		}
		a.permAsk = p
		return a.notifyIfAway("zemi: " + permTitle(req))
	})
	ans := <-ch
	return ans.ok, ans.by
}

func (a *App) answerPerm(ok bool, by string) {
	pa := a.permAsk
	if pa == nil || pa.answer == nil {
		return
	}
	waiting := a.host.AsksWaiting() > 1 // read first: the answer releases its ask
	pa.answer <- permAnswer{ok: ok, by: by}
	pa.answer = nil
	if !waiting {
		a.permAsk = nil
	}
	a.bump()
}

// dropAnsweredPerm closes an answered box once no ask waits to replace it.
func (a *App) dropAnsweredPerm() {
	if a.permAsk != nil && a.permAsk.answer == nil && a.host.AsksWaiting() == 0 {
		a.permAsk = nil
		a.bump()
	}
}

// chrome

// boxInner is the widest row a hostBox on a w-cell screen shows whole.
func boxInner(w int) int { return max(min(w-4, 100)-4, 1) }

// boxSize is how big a modal's box has been while it stays open. It only
// grows, so filtering or switching tabs never shrinks or moves the box; w
// counts the cells inside the padding.
type boxSize struct{ w, h int }

// listBoxW and textBoxW are the fewest cells inside a box of a list, and of
// a box that reads as text.
const listBoxW, textBoxW = 56, 72

// promptBox is the frame every modal draws in; body ends in its hint row.
// Blank rows before the hint pad it to size's height, within h rows, and
// its rows pad to size's width; size then grows to the box drawn.
func promptBox(title, badge string, body [][]hostapi.Segment, w, h, maxW int, size *boxSize) [][]hostapi.Segment {
	if n := min(size.h, h) - len(body) - 2; n > 0 && len(body) > 0 {
		body = slices.Insert(body, len(body)-1, make([][]hostapi.Segment, n)...)
	}
	if size.h > 0 { // a kept box cuts a longer title (a detail view's breadcrumb) instead of widening
		title = ansi.Truncate(title, size.w-1-map[bool]int{true: ansi.StringWidth(badge) + 2}[badge != ""], "…")
	}
	box := hostBox(title, hostapi.ChromeTitle, badge, body, w, maxW, size.w)
	size.w, size.h = max(size.w, hostapi.SegsWidth(box[0])-4), max(size.h, len(box))
	return box
}

// stack joins a box's non-empty blocks with one blank row between them; a
// short box (tight) drops those rows before anything else.
func stack(tight bool, blocks ...[][]hostapi.Segment) [][]hostapi.Segment {
	var out [][]hostapi.Segment
	for _, b := range blocks {
		if len(b) == 0 {
			continue
		}
		if len(out) > 0 && !tight {
			out = append(out, nil)
		}
		out = append(out, b...)
	}
	return out
}

// fitBox builds a box's rows at the loosest level they fit h rows at, the
// borders included: 0 keeps every row, 1 drops the blank rows between
// blocks, 2 also cuts each choice to its first row. render reports whether
// a viewport got fewer rows than it wants; at level 2 the viewports take
// what is left, one row at least.
func fitBox(h int, render func(level int) ([][]hostapi.Segment, bool)) [][]hostapi.Segment {
	for level := 0; ; level++ {
		if rows, squeezed := render(level); !squeezed && len(rows)+2 <= h || level == 2 {
			return rows
		}
	}
}

// oneRow is a block's placeholder for a viewport while the rows around it
// are counted.
var oneRow = [][]hostapi.Segment{nil}

// scrollChoiceBody is a prompt's body fit to h rows: the head rows (a tab
// strip), text scrolled in v at textW, the choice rows act draws (compact
// once the box squeezes; nil for a prompt whose keys ride the hint row), and
// a hint row w wide with the page keys while the text overflows and no line
// types, since a typed line keeps ctrl+u.
func scrollChoiceBody(h, w int, head [][]hostapi.Segment, v *viewport, text func(int) [][]hostapi.Segment, textW int, act func(compact bool) [][]hostapi.Segment, typing bool, hints ...hintPair) [][]hostapi.Segment {
	return fitBox(h, func(level int) ([][]hostapi.Segment, bool) {
		tight := level > 0
		var list [][]hostapi.Segment
		if act != nil {
			list = act(level > 1)
		}
		v.viewH = max(h-2-len(stack(tight, head, oneRow, list, oneRow))+1, 1)
		rows := v.window(text(textW), textW)
		pairs := hints
		if !typing {
			pairs = slices.Concat(hints, scrolls(*v))
		}
		hintRow := hint(w, pairs...)
		return stack(tight, head, rows, list, [][]hostapi.Segment{hintRow}), v.viewH < min(v.rows, 3)
	})
}

// wrapText wraps text to w cells. It splits the line breaks text already
// holds: a newline measures as no cell, so wrapping without splitting leaves
// one long row that spills past the box.
func wrapText(text, hl string, w int) [][]hostapi.Segment {
	var rows [][]hostapi.Segment
	for l := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		rows = append(rows, hostapi.WrapSegs([]hostapi.Segment{{Text: l, HL: hl}}, w)...)
	}
	return rows
}

// textRows keeps a box's text built per width: a box measures its text
// before it windows it, and markdown is too slow to render twice a frame.
type textRows map[int][][]hostapi.Segment

// at is build(w), built once per width.
func (t *textRows) at(build func(int) [][]hostapi.Segment) func(int) [][]hostapi.Segment {
	return func(w int) [][]hostapi.Segment {
		if *t == nil {
			*t = textRows{}
		}
		rows, ok := (*t)[w]
		if !ok {
			rows = build(w)
			(*t)[w] = rows
		}
		return rows
	}
}

// promptChoice is one answer of a Go-owned prompt.
type promptChoice struct{ key, label, desc, hl string }

// choiceRows draws a prompt's answers as buttons: "[y] Trust", the key right
// next to what it does, the selected one marked like a selected list row;
// a choice with no key keeps the column blank. Labels and descriptions are two
// columns, each wrapping in its own, so every description starts at one x.
// Every row carries the choice's 1-based Hit, so a click answers it. A label
// wraps, since the user answers what it says, unless short cuts each choice
// to one row.
func choiceRows(choices []promptChoice, sel, w int, short bool) [][]hostapi.Segment {
	kw, lw := 0, 0
	for _, c := range choices {
		kw = max(kw, ansi.StringWidth(c.key))
		if c.desc != "" {
			lw = max(lw, ansi.StringWidth(c.label))
		}
	}
	textW := max(w-kw-5, 8)
	lw = min(lw, textW/2)
	descW := textW - lw - 2
	var rows [][]hostapi.Segment
	for i, c := range choices {
		pad := strings.Repeat(" ", kw-ansi.StringWidth(c.key)+1)
		open, shut := "[", "]"
		if c.key == "" {
			open, shut = " ", " "
		}
		btn, textHL := []hostapi.Segment{
			{Text: "  " + open, HL: hostapi.ChromeHint}, {Text: c.key, HL: hostapi.ChromeHintKey}, {Text: shut + pad, HL: hostapi.ChromeHint},
		}, hostapi.ChromeValue
		if i == sel {
			btn, textHL = []hostapi.Segment{{Text: "› ", HL: hostapi.ChromeTitle}, {Text: open, HL: hostapi.ChromeHint}, {Text: c.key, HL: hostapi.ChromeHintKey}, {Text: shut + pad, HL: hostapi.ChromeHint}}, hostapi.ChromeBold
		}
		labelW, desc := textW, [][]hostapi.Segment(nil)
		if c.desc != "" {
			labelW, desc = lw, hostapi.WrapSegs([]hostapi.Segment{{Text: c.desc, HL: hostapi.ChromeHint}}, descW)
		}
		label := hostapi.WrapSegs([]hostapi.Segment{{Text: c.label, HL: cmp.Or(c.hl, textHL)}}, labelW)
		if short {
			label, desc = capRows(label, 1, labelW), capRows(desc, 1, descW)
		}
		for j := range max(len(label), len(desc)) {
			var r []hostapi.Segment
			if j < len(label) {
				r = label[j]
			}
			if j < len(desc) {
				r = slices.Concat(r, []hostapi.Segment{{Text: strings.Repeat(" ", max(lw+2-hostapi.SegsWidth(r), 2)), HL: hostapi.ChromeHint}}, desc[j])
			}
			row := slices.Concat(btn, r)
			if j > 0 {
				row = slices.Concat([]hostapi.Segment{{Text: strings.Repeat(" ", kw+5)}}, r)
			}
			for k := range row {
				row[k].Hit = i + 1
				if i == sel {
					row[k].Fill = hostapi.FillBand
				}
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// choiceView is choiceRows in v, whose viewH the caller sets, scrolled to
// keep the selected answer in view.
func choiceView(choices []promptChoice, sel int, v *viewport, w int, short bool) [][]hostapi.Segment {
	list := choiceRows(choices, sel, w, short)
	v.rows = len(list)
	top, end := len(list), 0
	for y, r := range list {
		if len(r) > 0 && r[0].Hit == sel+1 {
			top, end = min(top, y), y+1
		}
	}
	v.reveal(top, end)
	return v.window(list, w)
}

// hostBox draws a chrome box at most maxW wide whose rows pad to at least
// minW: a titled top border with an optional right-aligned badge, which
// wins over the title when space runs out. Title and badge come bare: the
// box pads each with a space and keeps a rule cell before the corner. A
// body segment with no style is value text, so a box shows one white.
func hostBox(title, titleHL, badge string, body [][]hostapi.Segment, w, maxW, minW int) [][]hostapi.Segment {
	label := func(s string, room int) string {
		if s == "" || room < 3 {
			return ""
		}
		return " " + ansi.Truncate(s, room-2, "…") + " "
	}
	bw := max(3+ansi.StringWidth(label(title, 999)+label(badge, 999)), minW+4)
	for _, r := range body {
		r, _ = cutBar(r)
		bw = max(bw, hostapi.SegsWidth(r)+4)
	}
	// wide boxes read badly; long rows end in an ellipsis instead
	bw = max(min(bw, w-4, maxW), min(12, max(w, 4)))
	badge = label(badge, bw-3)
	title = label(title, bw-3-ansi.StringWidth(badge))
	fill := bw - 2 - ansi.StringWidth(title) - ansi.StringWidth(badge)
	box := [][]hostapi.Segment{{
		{Text: "┌", HL: hostapi.ChromeRule, NoCopy: true, Line: true},
		{Text: title, HL: titleHL},
		{Text: strings.Repeat("─", fill), HL: hostapi.ChromeRule, NoCopy: true, Line: true},
		{Text: badge, HL: hostapi.ChromeHint},
		{Text: "┐", HL: hostapi.ChromeRule, NoCopy: true, Line: true},
	}}
	for _, r := range body {
		r, bar := cutBar(r)
		edge := []hostapi.Segment{{Text: " │", HL: hostapi.ChromeRule, NoCopy: true, Line: true}}
		if len(bar) > 0 {
			// the bar draws in the border: the thumb over it, the track the border itself
			b := bar[0]
			if b.Text == "│" {
				b.HL, b.Line = hostapi.ChromeRule, true
			}
			edge = []hostapi.Segment{{Text: " ", NoCopy: true}, b}
		}
		line := ellipsizeSegs(r, bw-4)
		row := make([]hostapi.Segment, 0, len(line)+4)
		row = append(row, hostapi.Segment{Text: "│ ", HL: hostapi.ChromeRule, NoCopy: true, Line: true})
		for _, s := range line {
			s.HL = cmp.Or(s.HL, hostapi.ChromeValue)
			row = append(row, s)
		}
		// a selected row's band runs to the box's edge, however wide its text
		fill := hostapi.FillAuto
		if len(line) > 0 && line[len(line)-1].Fill == hostapi.FillBand {
			fill = hostapi.FillBand
		}
		row = append(row, hostapi.Segment{Text: strings.Repeat(" ", max(0, bw-4-hostapi.SegsWidth(line))), NoCopy: true, Fill: fill})
		box = append(box, append(row, edge...))
	}
	box = append(box, []hostapi.Segment{{Text: "└" + strings.Repeat("─", bw-2) + "┘", HL: hostapi.ChromeRule, NoCopy: true, Line: true}})
	return onPanel(box)
}

// onPanel puts rows on the panel, all but what sets a surface of its own.
func onPanel(rows [][]hostapi.Segment) [][]hostapi.Segment {
	for _, r := range rows {
		for i := range r {
			if r[i].Fill == hostapi.FillAuto {
				r[i].Fill = hostapi.FillPanel
			}
		}
	}
	return rows
}

// cutBar splits a viewport row from the bar cell window put after it.
func cutBar(r []hostapi.Segment) (row, bar []hostapi.Segment) {
	if n := len(r); n > 0 && r[n-1].Bar {
		return r[:n-1], r[n-1:]
	}
	return r, nil
}

// ellipsizeSegs cuts a row to w cells ending in "…", at a word boundary
// when one is close. Trailing blanks are padding and cut without one.
func ellipsizeSegs(segs []hostapi.Segment, w int) []hostapi.Segment {
	if w < 2 || ansi.StringWidth(strings.TrimRight(hostapi.SegsText(segs), " ")) <= w {
		return truncateSegs(segs, w)
	}
	out := truncateSegs(segs, w-1)
	if n := len(out); n > 0 {
		last := out[n-1]
		if i := strings.LastIndex(last.Text, " "); i > 0 && ansi.StringWidth(last.Text[i:]) <= 12 {
			last.Text = strings.TrimRight(last.Text[:i], " ,;:-·.")
		}
		dots := last
		dots.Text = "…"
		out = append(out[:n-1:n-1], last, dots)
	}
	return out
}

// tailSegs cuts segs wider than w cells from the left behind an ellipsis,
// keeping the end, where a path's name is.
func tailSegs(segs []hostapi.Segment, w int) []hostapi.Segment {
	sw := hostapi.SegsWidth(segs)
	if w < 2 || sw <= w {
		return truncateSegs(segs, w)
	}
	out := dropCells(segs, sw-w+1)
	dots := out[0]
	dots.Text = "…"
	return append([]hostapi.Segment{dots}, out...)
}

// listItem is one selectable entry of a list.
type listItem struct {
	label, desc string
	key         hostapi.Segment // right-aligned column: a tag in Host:HintKey or a switchSeg
	section     string          // a change of section starts with a header row
	mark, head  hostapi.Segment // the state mark ahead of the label, and ahead of its section's header
	more        [][]hostapi.Segment
	segs        []hostapi.Segment   // the label styled; label keeps its plain text
	match       []int               // byte offsets of the label's runes a filter matched
	row         hostapi.OverlayItem // an overlay row's fields (overlayState)
}

// markSeg is a hostapi.Marks state mark and its gap, as the chat draws it;
// none for "".
func markSeg(kind string) hostapi.Segment {
	m, ok := hostapi.Marks[kind]
	if !ok {
		return hostapi.Segment{}
	}
	return hostapi.Segment{Text: m.Glyph + " ", HL: m.HL, NoCopy: true, Effect: map[bool]string{true: "spin"}[kind == "spin"]}
}

// tagSeg is a key column tag: a word that names, never a binary state.
func tagSeg(s string) hostapi.Segment { return hostapi.Segment{Text: s, HL: hostapi.ChromeHintKey} }

// switchSeg is the one on/off switch: 5 cells in every state, so a toggle
// never moves text, and a filled or hollow diamond and the word read without
// color, square shapes being the state marks'. Locked is on with nothing to
// switch.
func switchSeg(on, locked bool) hostapi.Segment {
	switch {
	case locked:
		return hostapi.Segment{Text: "  on ", HL: hostapi.ChromeHint}
	case on:
		return hostapi.Segment{Text: "◆ on ", HL: hostapi.ChromeOk}
	}
	return hostapi.Segment{Text: "◇ off", HL: hostapi.ChromeHint}
}

// listRows is the one list renderer: the selected item gets "› ", bold text
// and the band; more rows, wrapped by the caller, follow the label; every row
// carries its item's 1-based Hit. Past room rows it windows whole items in v
// (nil keeps no position), which moves only when the selection leaves it.
// Rows fit maxW cells when it is set, the bar included.
func listRows(items []listItem, sel int, v *viewport, room, maxW int) [][]hostapi.Segment {
	return listRowsW(items, items, sel, v, room, 0, maxW)
}

// listRowsW is listRows padded to at least minW cells, with a key column
// always showing. Descriptions line up in one column after the widest
// label that has one, set by all, the unfiltered list, so filtering never
// moves it; when a row does not fit its description gives way before any
// label is cut.
func listRowsW(items, all []listItem, sel int, v *viewport, room, minW, maxW int) [][]hostapi.Segment {
	var rows [][]hostapi.Segment
	keys := map[int]hostapi.Segment{}             // row -> key column
	cut, starts := map[int]bool{}, map[int]bool{} // rows a scrolled window may end after, and start at
	var selTop, bandTop, selEnd int
	labelW, keyW, tailDesc := 0, 0, 0
	for _, it := range all {
		if it.desc != "" { // a row with no description needs no column
			labelW = max(labelW, ansi.StringWidth(it.mark.Text+it.label))
		}
		if it.desc != "" && it.row.Tail {
			tailDesc = max(tailDesc, ansi.StringWidth(it.desc))
		}
		if it.key.Text != "" {
			keyW = max(keyW, 2+ansi.StringWidth(it.key.Text))
		}
	}
	// too narrow for a description column beside the widest label, every
	// description goes under its label instead of giving way
	under := maxW > 0 && labelW > maxW-7
	if maxW > 0 {
		// the marker and the gap before a description take 2 cells each
		labelW = max(min(labelW, maxW-4-keyW), 1)
		if tailDesc > 0 { // a tail label cut further keeps its name, so it gives way first
			labelW = max(min(labelW, maxW-4-keyW-tailDesc), min(labelW, 20))
		}
	}
	section := ""
	for i, it := range items {
		if len(rows) > 0 && (it.section != section || len(it.more) > 0 || len(items[i-1].more) > 0) {
			rows = append(rows, nil)
		}
		start := len(rows)
		starts[start] = true
		if it.section != section && it.section != "" {
			head := []hostapi.Segment{{Text: it.section, HL: hostapi.ChromeTitle}}
			if it.head.Text != "" {
				head = append([]hostapi.Segment{it.head}, head...)
			}
			rows = append(rows, head)
		}
		top := len(rows)
		section = it.section
		hit, hl, marker := i+1, hostapi.ChromeValue, hostapi.Segment{Text: "  "}
		if i == sel {
			hl, marker = hostapi.ChromeBold, hostapi.Segment{Text: "› ", HL: hostapi.ChromeTitle}
		}
		// segments without hl take the row's, and inline colors bold with it
		styled := func(r []hostapi.Segment) []hostapi.Segment {
			row := make([]hostapi.Segment, 0, len(r))
			for _, s := range r {
				switch {
				case s.HL == "":
					s.HL = hl
				case i == sel && strings.HasPrefix(s.HL, hostapi.SGRPrefix):
					s.HL += ";1" // markdown text is colored inline: bold it the same way
				}
				row = append(row, s)
			}
			return row
		}
		var itemRows [][]hostapi.Segment
		if it.label != "" || len(it.more) == 0 {
			row := it.segs
			if row == nil {
				row = []hostapi.Segment{{Text: it.label}}
			}
			row = styled(matchSegs(row, it.match, i == sel))
			if it.mark.Text != "" {
				row = append([]hostapi.Segment{it.mark}, row...)
			}
			descW := -1 // no cut
			kw := map[bool]int{true: 2 + ansi.StringWidth(it.key.Text)}[it.key.Text != ""]
			cut := ellipsizeSegs
			if it.row.Tail {
				cut = tailSegs
			}
			switch {
			case maxW > 0 && it.desc != "" && !under:
				row = cut(row, labelW)
				descW = maxW - 4 - labelW - kw
			case maxW > 0 && it.row.Tail:
				row = tailSegs(row, maxW-2-kw) // before the row's cut, which keeps the start
			}
			if it.desc != "" && !under && (descW < 0 || descW > 2) {
				desc := []hostapi.Segment{{Text: it.desc, HL: hostapi.ChromeHint}}
				if descW > 0 {
					desc = ellipsizeSegs(desc, descW)
				}
				row = append(row, hostapi.Segment{Text: strings.Repeat(" ", max(0, labelW-hostapi.SegsWidth(row))+2)})
				row = append(row, desc...)
			}
			if it.key.Text != "" {
				it.key.Hit = hit
				keys[len(rows)] = it.key
			}
			itemRows = append(itemRows, row)
			if under && it.desc != "" {
				itemRows = append(itemRows, ellipsizeSegs([]hostapi.Segment{{Text: it.desc, HL: hostapi.ChromeHint}}, maxW-2))
			}
		}
		for _, r := range it.more {
			itemRows = append(itemRows, styled(r))
		}
		for j, r := range itemRows {
			g := hostapi.Segment{Text: "  "}
			if j == 0 {
				g = marker
			}
			row := append([]hostapi.Segment{g}, r...)
			for k := range row {
				row[k].Hit = hit
			}
			rows = append(rows, row)
		}
		cut[len(rows)-1] = true
		if i == sel {
			selTop, bandTop, selEnd = start, top, len(rows)
		}
	}
	if v == nil {
		v = &viewport{}
	}
	v.rows, v.viewH = len(rows), max(room, 1)
	// Every row pads to the widest of the whole list, so a box sized from
	// the visible window keeps its width while scrolling.
	widest := minW
	if maxW > 0 { // a width remembered from a wider screen
		widest = min(widest, maxW)
	}
	for y, r := range rows {
		k, ok := keys[y]
		kw := map[bool]int{true: 2 + ansi.StringWidth(k.Text)}[ok]
		if maxW > 0 && hostapi.SegsWidth(r)+kw > maxW {
			// a cut row still wants every cell, so the list keeps them all
			// where the word-boundary cut would leave a few
			rows[y], widest = ellipsizeSegs(r, maxW-kw), maxW
			continue
		}
		widest = max(widest, hostapi.SegsWidth(r)+kw)
	}
	for y, r := range rows {
		if k, ok := keys[y]; ok {
			pad := hostapi.Segment{Text: strings.Repeat(" ", widest-hostapi.SegsWidth(r)-ansi.StringWidth(k.Text)), Hit: k.Hit}
			rows[y] = append(r, pad, k)
		} else {
			rows[y] = append(r, hostapi.Segment{Text: strings.Repeat(" ", widest-hostapi.SegsWidth(r)), NoCopy: true})
		}
		if y >= bandTop && y < selEnd {
			for k := range rows[y] {
				rows[y][k].Fill = hostapi.FillBand
			}
		}
	}
	if !v.overflows() {
		v.top = 0
		return rows
	}
	v.reveal(selTop, selEnd)
	for v.top < selTop && !starts[v.top] {
		v.top++
	}
	// start and end the window on whole items, so no item shows cut, no
	// blank separator shows at an edge and no header shows without its
	// first item; an item taller than the window is cut anyway
	end := v.top + v.viewH
	for end > v.top+1 && !cut[end-1] {
		end--
	}
	if !cut[end-1] {
		end = v.top + v.viewH
	}
	win := slices.Clone(rows)
	for j := end; j < v.top+v.viewH; j++ {
		if j < len(win) {
			win[j] = nil
		} else {
			win = append(win, nil) // the last item pushed the window past the end
		}
	}
	return v.window(win, widest)
}

// matchSegs draws the runes at byte offsets pos of a row's text, a filter's
// match, before the row's style fills the segments without one: in the
// accent, bold on the selected row.
func matchSegs(segs []hostapi.Segment, pos []int, sel bool) []hostapi.Segment {
	if len(pos) == 0 {
		return segs
	}
	hl := map[bool]string{false: hostapi.ChromeHintKey, true: hostapi.ChromeTitle}[sel]
	var out []hostapi.Segment
	j, off := 0, 0
	for _, s := range segs {
		start, was := 0, false
		flush := func(end int) {
			if end > start {
				run := s
				run.Text = s.Text[start:end]
				if was {
					run.HL = hl
				}
				out = append(out, run)
			}
			start = end
		}
		for i := range s.Text {
			for j < len(pos) && pos[j] < off+i {
				j++
			}
			if hit := j < len(pos) && pos[j] == off+i; hit != was {
				flush(i)
				was = hit
			}
		}
		flush(len(s.Text))
		off += len(s.Text)
	}
	return out
}

// shimmer is the "shimmer" segment effect: a bright band sweeps across the
// text every 2s, each rune blending the segment's color toward Normal by
// its distance to the band center. Blends are quantized to 8 steps so the
// sgr style cache stays small.
func (a *App) shimmer(seg hostapi.Segment) []hostapi.Segment {
	dim, fg := a.thm.style(seg.HL).Fg, a.thm.style("Normal").Fg
	if !a.host.Settings.Bool("ui.animations") || dim == nil || fg == nil {
		return []hostapi.Segment{{Text: seg.Text, HL: seg.HL}}
	}
	const band = 3.0
	runes := []rune(seg.Text)
	pos := float64(a.frame()*frameMS%2000)/2000*(float64(len(runes))+2*band) - band
	out := make([]hostapi.Segment, 0, len(runes))
	for i, r := range runes {
		k := 0.0
		if d := math.Abs(float64(i) - pos); d < band {
			k = math.Round((1+math.Cos(math.Pi*d/band))/2*8) / 8
		}
		cr, cg, cb, _ := hostapi.Mix(dim, fg, k).RGBA()
		hl := fmt.Sprintf("%s38;2;%d;%d;%d", hostapi.SGRPrefix, cr>>8, cg>>8, cb>>8)
		out = append(out, hostapi.Segment{Text: string(r), HL: hl})
	}
	return out
}

// permBox draws the permission prompt: the question, the facts table
// (what it acts on, why it asked, the model's own words), the command or
// the diff it would apply, and the answers. Read down the left edge it
// answers what, where and why before the keys.
func (a *App) permBox(pa *permAskPrompt, w, h int) [][]hostapi.Segment {
	req := pa.req
	isDiff := req.Subject == perm.SubjectWrite && req.Preview != ""
	maxW := 96
	if isDiff {
		maxW = 120
	}
	inner := max(min(w-4, maxW)-4, 1)
	target, why, says := permFacts(req, a.host.Dirs.ProjectDir, inner, h)
	var content [][]hostapi.Segment
	switch {
	case req.Subject == perm.SubjectShell:
		content = a.codePanel(req.Arg, "bash", inner)
	case isDiff:
		content = hostapi.DiffRows(req.Preview, inner)
		for _, r := range content {
			for i := range r {
				r[i].HL = hostapi.ChromePrefix + r[i].HL // the chrome's Diff* styles
			}
		}
	case req.Preview != "":
		content = a.codePanel(req.Preview, previewLang(req), inner)
	}
	var btns []promptChoice
	for _, c := range pa.choices() {
		btns = append(btns, c.promptChoice)
	}
	hints := []hintPair{pickHint, {"esc", "deny"}}
	placeholder := oneRow
	if len(content) == 0 {
		placeholder = nil
	}
	var list [][]hostapi.Segment
	var tight bool
	nWhy, nSays := len(why), len(says)
	fitBox(h, func(level int) ([][]hostapi.Segment, bool) {
		tight, list = level > 0, choiceRows(btns, pa.sel, inner, level > 1)
		// the panel keeps two rows before the model's words give way, then
		// the reason; the target, the answers and the hint never do
		room := h - 2 - len(stack(tight, placeholder, list, oneRow)) + len(placeholder)
		left := func() int {
			facts := len(target) + nWhy + nSays
			return room - facts - map[bool]int{true: 1}[facts > 0 && !tight]
		}
		keep := min(len(content), 2)
		nWhy, nSays = len(why), len(says)
		for nSays > 0 && left() < keep {
			nSays--
		}
		for nWhy > 0 && left() < keep {
			nWhy--
		}
		pa.view.viewH = max(left(), 1)
		return nil, left() < keep || nWhy < len(why) || nSays < len(says)
	})
	facts := slices.Concat(target, capRows(why, nWhy, inner), capRows(says, nSays, inner))
	if pa.answer == nil {
		dimRows(list) // answered: no longer a target
	}
	if len(content) > pa.view.viewH {
		hints = append(hints, scrollHint)
	}
	hintRow := hint(inner, hints...)
	// panels span the box, and the box fits its content: a padded panel
	// counts by its text, other rows whole as hostBox counts them
	fit := listBoxW
	for _, r := range content {
		fit = max(fit, ansi.StringWidth(strings.TrimRight(hostapi.SegsText(r), " ")))
	}
	for _, r := range slices.Concat(facts, list, [][]hostapi.Segment{hintRow}) {
		fit = max(fit, hostapi.SegsWidth(r))
	}
	fit = min(fit, inner)
	for i := range content {
		content[i] = padPanel(content[i], fit)
	}
	content = pa.view.window(content, fit)
	// who asks, whose code runs, and how many asks wait (an upper bound: a
	// waiting ask may settle without a prompt)
	badge := []string{req.Agent, req.Owner, req.Script}
	if n := a.host.AsksWaiting(); n > 1 {
		badge = append(badge, fmt.Sprintf("1 of %d", n))
	}
	body := stack(tight, facts, content, list, [][]hostapi.Segment{hintRow})
	// the title says where a yes runs, so the badge gets what it leaves, a
	// third of the box at least
	title, bw := permTitle(req), min(w-4, maxW)
	who := ansi.Truncate(strings.Join(slices.DeleteFunc(badge, func(s string) bool { return s == "" }), " · "), max(bw-ansi.StringWidth(title)-7, bw/3), "…")
	return promptBox(title, who, body, w, h, maxW, &pa.size)
}

// capRows keeps the first n of wrapped rows, ending the last kept one in
// "…" when rows were dropped.
func capRows(rows [][]hostapi.Segment, n, w int) [][]hostapi.Segment {
	if n <= 0 {
		return nil
	}
	if len(rows) <= n {
		return rows
	}
	last := truncateSegs(rows[n-1], w-1)
	if k := len(last); k > 0 {
		end := last[k-1]
		end.Text = strings.TrimRight(end.Text, " ") + "…"
		last = append(last[:k-1:k-1], end)
	}
	return append(rows[:n-1:n-1], last)
}

// padPanel cuts or extends a panel row to width, keeping its last style.
func padPanel(r []hostapi.Segment, width int) []hostapi.Segment {
	r = truncateSegs(r, width)
	if n := len(r); n > 0 && hostapi.SegsWidth(r) < width {
		last := r[n-1]
		last.Text += strings.Repeat(" ", width-hostapi.SegsWidth(r))
		r = append(r[:n-1:n-1], last)
	}
	return r
}

// runsTitle asks about a command where a yes runs it.
//
//exhaustive:enforce
var runsTitle = map[hostapi.RunsIn]string{
	hostapi.RunsOutside:   "Run this command outside the sandbox?",
	hostapi.RunsNoSandbox: "Run this command?",
	hostapi.RunsNetwork:   "Run this command in the sandbox, with the network?",
	hostapi.RunsConfined:  "Run this command in the sandbox?",
}

// permTitle is the question a request asks, from its typed fields.
func permTitle(req hostapi.PermRequest) string {
	switch server, mcp := strings.CutPrefix(req.Subject, "mcp."); {
	case req.Subject == perm.SubjectShell:
		return cmp.Or(runsTitle[req.Where], "Run this command?")
	case req.Subject == perm.SubjectRead && req.Tool == tools.ToolSkill:
		return "Load this skill?"
	case req.Subject == perm.SubjectRead:
		return "Read this file?"
	case req.Tool == tools.ToolWrite:
		return "Write this file?"
	case req.Tool == tools.ToolEdit:
		return "Edit this file?"
	case req.Subject == perm.SubjectWrite:
		return "Change this file?"
	case strings.HasPrefix(req.Subject, "net.") && req.Tool == tools.ToolWebSearch:
		return "Search the web?"
	case strings.HasPrefix(req.Subject, "net."):
		return "Fetch this URL?"
	case mcp:
		return "Use " + req.Arg + " from " + server + "?"
	case req.Subject == perm.SubjectLua:
		return "Run Lua inside zemi?"
	case req.Subject == perm.SubjectWorkflow:
		return "Run this workflow?"
	case req.Subject == "tool."+req.Tool:
		return "Use the " + req.Tool + " tool?"
	case req.Subject == "memory.write":
		return "Forget this memory?"
	}
	return "Allow " + req.Subject + "?"
}

// permFacts is the facts table under the prompt's title, three blocks
// sharing one name column: the target (the programs a command runs, a file,
// a URL), why zemi asked, and the model's own words, quoted and last since
// nothing checked them. why and says take two rows at most, a target a
// third of the screen, keeping its first and last rows.
func permFacts(req hostapi.PermRequest, root string, w, h int) (target, why, says [][]hostapi.Segment) {
	name, val := permTarget(req, root)
	// a command's programs, unless it is only shell syntax (set -e; true)
	runs := req.Subject == perm.SubjectShell && (req.Runs == nil || len(req.Runs) > 0)
	if runs {
		name = "runs"
	}
	kw := 0
	for n, on := range map[string]bool{name: val != nil || runs, "why": req.Reason != "", "says": req.Description != ""} {
		if on {
			kw = max(kw, ansi.StringWidth(n)+2)
		}
	}
	switch {
	case runs && w-kw < 20:
		target = append(fieldLine(name, nil, w), runsRows(req.Runs, w)...)
	case runs:
		target = fieldRow(name, kw, runsRows(req.Runs, w-kw))
	case val != nil:
		target = field(name, kw, val, w)
		if n := max(h/3, 3); len(target) > n {
			more := fieldRow("", kw, [][]hostapi.Segment{{{Text: hostapi.MoreLines(len(target) - n + 1), HL: hostapi.ChromeHint}}})
			target = slices.Concat(target[:n-2], more, target[len(target)-1:])
		}
	}
	if req.Reason != "" {
		why = capRows(field("why", kw, []hostapi.Segment{{Text: sanitize.Line(req.Reason), HL: hostapi.ChromeValue}}, w), 2, w)
	}
	if req.Description != "" {
		says = capRows(field("says", kw, []hostapi.Segment{{Text: `"` + sanitize.Line(req.Description) + `"`, HL: hostapi.ChromeHint}}, w), 2, w)
	}
	return target, why, says
}

// permTarget is the facts table's first row: what a request acts on, as
// its name and value; no value when the title names it (a tool, a memory)
// or for a command, whose runs row permFacts lays out.
func permTarget(req hostapi.PermRequest, root string) (string, []hostapi.Segment) {
	// a path or URL wraps at its slashes: its file name and its query are
	// what the user approves
	parts := func(s string) []hostapi.Segment {
		var segs []hostapi.Segment
		for _, p := range strings.SplitAfter(s, "/") {
			segs = append(segs, hostapi.Segment{Text: p, HL: hostapi.ChromeValue})
		}
		return segs
	}
	switch arg := req.Arg; {
	case req.Subject == perm.SubjectShell:
		return "", nil
	case tools.PathSubject(req.Subject), req.Subject == perm.SubjectLua && arg != "script":
		if rel, ok := perm.Under(root, arg); ok && root != "" && filepath.IsAbs(arg) {
			arg = rel
		}
		return "file", parts(arg)
	case strings.HasPrefix(req.Subject, "net."):
		// the host is what the session answer allows and where a look-alike hides
		if u, err := url.Parse(arg); err == nil && u.Host != "" {
			before, after, _ := strings.Cut(arg, u.Host)
			return "url", slices.Concat([]hostapi.Segment{{Text: before, HL: hostapi.ChromeValue}, {Text: u.Host, HL: hostapi.ChromeBold}}, parts(after))
		}
		return "url", parts(arg)
	case req.Subject == perm.SubjectWorkflow && arg != "script":
		return "workflow", []hostapi.Segment{{Text: arg, HL: hostapi.ChromeValue}}
	case strings.HasPrefix(req.Subject, "mcp."), req.Subject == "tool."+req.Tool, req.Subject == "memory.write", arg == "script":
		return "", nil
	}
	return "arg", parts(req.Arg)
}

// riskRank and riskHL order and color the marks of the runs row.
var (
	riskRank = map[perm.Risk]int{perm.RiskDeletes: 2, perm.RiskCode: 1}
	riskHL   = map[perm.Risk]string{perm.RiskDeletes: hostapi.ChromeDanger, perm.RiskCode: hostapi.ChromeWarn}
)

// runsRows lays out what a command runs in at most two rows w wide, each
// name whole: marked names first (deletes in Danger, runs code in Warn), so
// a cut never hides one, and the names that do not fit counted as "… N
// more". nil runs is a list the parse could not vouch for.
func runsRows(runs []perm.Program, w int) [][]hostapi.Segment {
	if runs == nil {
		return [][]hostapi.Segment{{{Text: "unknown: read the command", HL: hostapi.ChromeWarn}}}
	}
	runs = slices.Clone(runs)
	slices.SortStableFunc(runs, func(a, b perm.Program) int { return riskRank[b.Risk] - riskRank[a.Risk] })
	items := make([]hostapi.Segment, len(runs))
	for i, p := range runs {
		items[i] = hostapi.Segment{Text: p.Name, HL: hostapi.ChromeValue}
		if p.Risk != "" {
			items[i] = hostapi.Segment{Text: p.Name + " (" + string(p.Risk) + ")", HL: riskHL[p.Risk]}
		}
	}
	for n := len(items); ; n-- {
		shown := items[:n]
		if n < len(items) {
			shown = append(slices.Clone(shown), hostapi.Segment{Text: fmt.Sprintf("… %d more", len(items)-n), HL: hostapi.ChromeHint})
		}
		var rows [][]hostapi.Segment
		var row []hostapi.Segment
		for _, it := range shown {
			switch {
			case len(row) == 0:
			case hostapi.SegsWidth(row)+3+ansi.StringWidth(it.Text) <= w:
				row = append(row, hostapi.Segment{Text: " · ", HL: hostapi.ChromeHint})
			default:
				rows, row = append(rows, row), nil
			}
			row = append(row, it)
		}
		if rows = append(rows, row); len(rows) <= 2 || n == 0 {
			return rows
		}
	}
}

// hintPair is one key of a footer hint and the verb it does.
type hintPair struct{ key, verb string }

// enter's hint in every prompt, in the lists' words (enterVerb): one verb
// for every button and one for typed text, as wide as "pick" and "open",
// since a changing width shifts the row.
var pickHint, sendHint = hintPair{"enter", "pick"}, hintPair{"enter", "send"}

// hintSlot is where a pair sits in a footer: moving, enter, space, copy,
// letter keys, esc, then scrolling. A screen's letter keys come and go with
// its state (a workflow's run ending), so they sit late and their going
// moves only esc; the scroll hint comes and goes as the text does, so it
// sits last and its cells are trailing blanks while it is away.
func hintSlot(p hintPair) int {
	switch {
	case p.verb == "next" || p.verb == "previous":
		return 0
	case p.key == "enter":
		return 1
	case p.key == "space":
		return 2
	case p.key == "y":
		return 3
	case p.key == "esc":
		return 5
	case p == scrollHint || p == scrollGap:
		return 6
	}
	return 4
}

// hint is the one footer row every box draws: "enter pick · esc deny",
// the pairs in slot order. Past w cells the middle pairs give way,
// scrolling and copy first, then the last first; enter and esc never do.
func hint(w int, pairs ...hintPair) []hostapi.Segment {
	pairs = slices.Clone(pairs)
	slices.SortStableFunc(pairs, func(a, b hintPair) int { return hintSlot(a) - hintSlot(b) })
	for {
		var out []hostapi.Segment
		for _, p := range pairs {
			switch {
			case p == scrollGap && len(out) > 0: // scrollHint's cells, its separator included
				out = append(out, hostapi.Segment{Text: strings.Repeat(" ", 4+ansi.StringWidth(scrollHint.key+scrollHint.verb))})
				continue
			case p == scrollGap:
				continue
			case len(out) > 0:
				out = append(out, hostapi.Segment{Text: " · ", HL: hostapi.ChromeHint})
			}
			out = append(out, hostapi.Segment{Text: p.key, HL: hostapi.ChromeHintKey}, hostapi.Segment{Text: " " + p.verb, HL: hostapi.ChromeHint})
		}
		i := slices.IndexFunc(pairs, func(p hintPair) bool { return p == scrollHint || p == scrollGap })
		if i < 0 {
			i = slices.IndexFunc(pairs, func(p hintPair) bool { return p.key == "y" })
		}
		if i < 0 {
			i = len(pairs) - 1
			for i >= 0 && (pairs[i].key == "enter" || pairs[i].key == "esc") {
				i--
			}
		}
		if hostapi.SegsWidth(out) <= w || i < 0 {
			return out
		}
		pairs = slices.Delete(pairs, i, i+1)
	}
}

// scrollHint names the half-page keys, which every box's text scrolls on
// and a laptop has.
var scrollHint = hintPair{"ctrl+d/u", "scroll"}

// scrollGap keeps scrollHint's cells blank in a list that can grow past its
// view, so the hint coming moves none of the keys after it.
var scrollGap = hintPair{verb: "scroll gap"}

// scrolls is scrollHint while any of views overflows.
func scrolls(views ...viewport) []hintPair {
	for _, v := range views {
		if v.overflows() {
			return []hintPair{scrollHint}
		}
	}
	return nil
}

// previewLang is the language a request's preview is written in: what the
// model will run, as the chat's rows highlight it; "" for prose.
func previewLang(req hostapi.PermRequest) string {
	switch {
	case req.Subject == perm.SubjectLua || req.Subject == perm.SubjectWorkflow:
		return "lua"
	case strings.HasPrefix(req.Subject, "mcp.") || strings.HasPrefix(req.Subject, "tool."):
		return "json" // a call's input
	}
	return ""
}

// codePanel shows text in a well of the canvas inside the box, wrapped to
// width and highlighted as lang when it names one.
func (a *App) codePanel(text, lang string, width int) [][]hostapi.Segment {
	var rows [][]hostapi.Segment
	if lang != "" {
		rows = a.host.Highlight(text, lang, width-2)
	} else {
		rows = wrapText(text, "", width-2)
	}
	out := make([][]hostapi.Segment, len(rows))
	for i, r := range rows {
		row := make([]hostapi.Segment, 0, len(r)+2)
		row = append(row, hostapi.Segment{Text: " ", Fill: hostapi.FillCanvas})
		w := 0
		for _, sg := range r {
			w += ansi.StringWidth(sg.Text)
			row = append(row, hostapi.Segment{Text: sg.Text, HL: cmp.Or(sg.HL, hostapi.ChromeValue), Fill: hostapi.FillCanvas})
		}
		row = append(row, hostapi.Segment{Text: strings.Repeat(" ", max(0, width-2-w)) + " ", Fill: hostapi.FillCanvas})
		out[i] = row
	}
	return out
}
