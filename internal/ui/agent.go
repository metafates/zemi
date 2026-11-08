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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/set"
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

// _permGrace swallows keys right after a prompt opens: someone typing a
// message should not approve a command by accident.
const _permGrace = 400 * time.Millisecond

// _askSwallowWindow is how long a key that answered an ask eats the keys
// behind it: a paste's tail arrives within it, a human's next key does not.
const _askSwallowWindow = 100 * time.Millisecond

// fresh reports whether a prompt opened within permGrace.
func (a *App) fresh(opened time.Time) bool {
	return a.program != nil && a.now().Sub(opened) < _permGrace
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
	return []permChoice{
		{key: "y", label: "Yes", by: "user:once", ok: true},
		{key: "n", label: "No", by: tools.ByUser},
		{key: "t", label: "No, and tell zemi", desc: "stop the turn and say what to do instead", by: tools.ByUser, tell: true},
	}
}

// choosePerm answers the prompt with c. Telling zemi what to do also denies
// the waiting asks and interrupts the turn; the composer already has focus.
func (a *App) choosePerm(c permChoice) {
	if a.permAsk == nil || a.permAsk.answer == nil {
		return // a click on the last frame's box after it was answered
	}
	if c.tell {
		a.host.DenyWaitingAsks()
		a.composerNote = "Tell zemi what to do instead…" // the draft is theirs to write
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

// DismissAsk implements hostapi.UIHooks: it answers the shown permission
// ask as nobody, the chat it belonged to gone (the writer's reset), so the
// waiter unblocks at the wipe and the turn ends with its honest interrupt
// instead of on the next key.
func (a *App) DismissAsk() {
	a.await(func() tea.Cmd {
		a.answerPerm(false, tools.ByNobody)
		return nil
	})
}

// AskPermission implements hostapi.UIHooks. It blocks the caller (a tool
// run on the turn goroutine) until the user answers the overlay; safe from
// any goroutine via the hook channel + buffered answer, mirroring AskTrust.
func (a *App) AskPermission(req hostapi.PermRequest) (bool, string) {
	// Every field carries model input: strip escapes before the chrome.
	req.Subject, req.Arg = sanitize.String(req.Subject), sanitize.String(req.Arg)
	req.Reason, req.Preview = sanitize.String(req.Reason), sanitize.String(req.Preview)
	req.Description = sanitize.String(req.Description)
	req.Agent = sanitize.Line(req.Agent)
	// a tool's input reads as its fields, one per line
	if strings.HasPrefix(req.Preview, "{") {
		if out, err := jsontext.AppendFormat(nil, req.Preview, jsontext.WithIndent("  ")); err == nil {
			req.Preview = string(out)
		}
	}
	ch := make(chan permAnswer, 1)
	a.await(func() tea.Cmd {
		p := &permAskPrompt{req: req, answer: ch, opened: a.now()}
		// the box opens on No, the answer a stray enter picks: the trust box
		// opens on Later for the same reason, and y is one key away
		for i, c := range p.choices() {
			if c.key == "n" {
				p.sel = i
				break
			}
		}
		if a.permAsk != nil { // the answered box: keep its size so nothing jumps
			p.size = a.permAsk.size
		}
		a.permAsk = p
		return a.notifyUnlessReported(permAlert(req))
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

// _listBoxW and _textBoxW are the fewest cells inside a box of a list, and of
// a box that reads as text.
const _listBoxW, _textBoxW = 56, 72

// promptBox is the frame every modal draws in; body ends in its hint row.
// Blank rows before the hint pad it to size's height, within h rows, and
// its rows pad to size's width; size then grows to the box drawn.
func promptBox(title, badge string, body [][]hostapi.Segment, w, h, maxW int, size *boxSize) [][]hostapi.Segment {
	if n := min(size.h, h) - len(body) - 2; n > 0 && len(body) > 0 {
		body = slices.Insert(body, len(body)-1, make([][]hostapi.Segment, n)...)
	}
	if size.h > 0 { // a kept box cuts a longer title (a detail view's breadcrumb) instead of widening
		badgeW := 0
		if badge != "" {
			badgeW = ansi.StringWidth(badge) + 2
		}
		title = ansi.Truncate(title, size.w-1-badgeW, "…")
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

// _oneRow is a block's placeholder for a viewport while the rows around it
// are counted.
var _oneRow = [][]hostapi.Segment{nil}

// scrollChoiceBody is a prompt's body fit to h rows: the head rows (a tab
// strip), text scrolled in v at textW, the choice rows act draws (compact
// once the box squeezes; nil for a prompt whose keys ride the hint row), and
// a hint row w wide naming the half-page keys while the text overflows, a
// typed line included, since they are the line's too.
func scrollChoiceBody(h, w int, head [][]hostapi.Segment, v *viewport,
	text func(int) [][]hostapi.Segment, textW int,
	act func(compact bool) [][]hostapi.Segment, hints ...hintPair,
) [][]hostapi.Segment {
	return fitBox(h, func(level int) ([][]hostapi.Segment, bool) {
		tight := level > 0
		var list [][]hostapi.Segment
		if act != nil {
			list = act(level > 1)
		}
		v.viewH = max(h-2-len(stack(tight, head, _oneRow, list, _oneRow))+1, 1)
		rows := v.window(text(textW), textW, _boxFade)
		hintRow := hint(w, slices.Concat(hints, scrolls(*v))...)
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
type promptChoice struct{ key, label, desc, hl, tag string }

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
	descW := textW - lw - 3
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
			btn, textHL = []hostapi.Segment{
				{Text: "› ", HL: hostapi.ChromeTitle},
				{Text: open, HL: hostapi.ChromeHint},
				{Text: c.key, HL: hostapi.ChromeHintKey},
				{Text: shut + pad, HL: hostapi.ChromeHint},
			}, hostapi.ChromeBold
		}
		labelW, desc := textW, [][]hostapi.Segment(nil)
		if c.desc != "" {
			labelW, desc = lw, hostapi.WrapSegs([]hostapi.Segment{{Text: c.desc, HL: hostapi.ChromeHint}}, descW)
		}
		labelSegs := []hostapi.Segment{{Text: c.label, HL: cmp.Or(c.hl, textHL)}}
		if c.tag != "" {
			labelSegs = append(labelSegs, hostapi.Segment{Text: "  " + c.tag, HL: hostapi.ChromeHint})
		}
		label := hostapi.WrapSegs(labelSegs, labelW)
		if short {
			label, desc = capRows(label, 1, labelW), capRows(desc, 1, descW)
		}
		for j := range max(len(label), len(desc)) {
			var r []hostapi.Segment
			if j < len(label) {
				r = label[j]
			}
			if j < len(desc) {
				if j < len(label) {
					// the description starts at the same column on every row:
					// label + pad + the two-cell gap a continuation aligns to
					pad := max(lw+1-hostapi.SegsWidth(r), 1)
					r = slices.Concat(r, []hostapi.Segment{{Text: strings.Repeat(" ", pad) + "  ", HL: hostapi.ChromeHint}}, desc[j])
				} else {
					// a wrapped description continues under the first row's
					// description: no seam, no label column to cross
					r = slices.Concat([]hostapi.Segment{{Text: strings.Repeat(" ", lw+3)}}, desc[j])
				}
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
// keep the selected answer in view, a fade's rows clear of the edge.
func choiceView(choices []promptChoice, sel int, v *viewport, w int, short bool) [][]hostapi.Segment {
	list := choiceRows(choices, sel, w, short)
	v.rows = len(list)
	top, end := len(list), 0
	for y, r := range list {
		if len(r) > 0 && r[0].Hit == sel+1 {
			top, end = min(top, y), y+1
		}
	}
	v.reveal(top-_boxFade, end+_boxFade)
	return v.window(list, w, _boxFade)
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
			// the bar draws in the border: the thumb over it, the track the border
			// itself, on the box's own surface, so the band of a selected row stops
			// at the cell before it instead of running into the border column
			b := bar[0]
			b.Fill = hostapi.FillAuto
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

// onPanel puts rows on the panel, all but what sets a surface of its own: a
// well of the canvas inside a box stays on the canvas, its fade included.
func onPanel(rows [][]hostapi.Segment) [][]hostapi.Segment {
	for _, r := range rows {
		for i := range r {
			if r[i].Fill == hostapi.FillAuto {
				r[i].Fill = hostapi.FillPanel
			}
			r[i].OnPanel = r[i].Fill != hostapi.FillCanvas
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

// _clipFragment is how many cells a fragment the cut leaves may take: one that
// short is dropped whole, so the ellipsis lands after a word rather than
// inside it.
const _clipFragment = 12

// ellipsizeSegs cuts a row to w cells ending in "…", at a word boundary
// when the cut splits one. Trailing blanks are padding and cut without one.
func ellipsizeSegs(segs []hostapi.Segment, w int) []hostapi.Segment {
	if w < 2 || ansi.StringWidth(strings.TrimRight(hostapi.SegsText(segs), " ")) <= w {
		return truncateSegs(segs, w)
	}
	out := truncateSegs(segs, w-1)
	if n := len(out); n > 0 {
		last := out[n-1]
		if i := strings.LastIndex(last.Text, " "); i > 0 && ansi.StringWidth(last.Text[i:]) <= _clipFragment && cutSplitWord(segs, len(out), last.Text) {
			last.Text = strings.TrimRight(last.Text[:i], " ,;:-·.")
		} else {
			last.Text = strings.TrimRight(last.Text, " ,;:-·.")
		}
		dots := last
		dots.Text = "…"
		out = append(out[:n-1:n-1], last, dots)
	}
	return out
}

// cutSplitWord reports whether the cut left the fragment after the last
// space partial: the text goes on with a word character where the kept text
// stops. One the cut left whole stays, so a fitting word is not dropped.
func cutSplitWord(segs []hostapi.Segment, kept int, text string) bool {
	if kept > len(segs) {
		return false
	}
	orig := segs[kept-1].Text
	if len(orig) > len(text) {
		r, _ := utf8.DecodeRuneInString(orig[len(text):])
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	}
	if kept < len(segs) { // the cut fell at the segment's end: the word may go on in the next one
		r, _ := utf8.DecodeRuneInString(segs[kept].Text)
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	}
	return false
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
	score       int                 // the match's rank in the popup's preselection, 0 for none
	row         hostapi.OverlayItem // an overlay row's fields (overlayState)
}

// markSeg is a hostapi.Marks state mark and its gap, as the chat draws it;
// none for "".
func markSeg(kind hostapi.MarkKind) hostapi.Segment {
	m, ok := hostapi.Marks[kind]
	if !ok {
		return hostapi.Segment{}
	}
	var fx hostapi.Effect
	if kind == tools.MarkSpin {
		fx = hostapi.EffectSpin
	}
	return hostapi.Segment{Text: m.Glyph + " ", HL: m.HL, NoCopy: true, Effect: fx}
}

// itemMark is a row's leading mark: its own glyph when it carries one, else
// the state mark its kind names. Either way it draws ahead of the label and
// stays out of it, so a filter matches the label alone.
func itemMark(item hostapi.OverlayItem) hostapi.Segment {
	if item.Glyph != "" {
		return hostapi.Segment{Text: item.Glyph + " ", NoCopy: true}
	}
	return markSeg(item.Mark)
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
	default:
		return hostapi.Segment{Text: "◇ off", HL: hostapi.ChromeHint}
	}
}

// listRowsW is the one list renderer: the selected item gets "› ", bold text
// and the band; more rows, wrapped by the caller, follow the label; every row
// carries its item's 1-based Hit. Past room rows it windows whole items in v
// (nil keeps no position), which moves only when the selection leaves it.
// Rows fit maxW cells when it is set, the bar included, and pad to at least
// minW, with a key column always showing. Descriptions line up in one column
// sized before the labels, over all, the unfiltered list, so filtering never
// moves it: when a box is too narrow for both, the longer column gives way -
// a label keeps its room while a description is the longer thing, and a
// description keeps its need while the label is (a tail path's end, whose
// desc is the only metadata beside it); only a box too narrow for either
// puts every description under its label.
// listNeedW measures a list's widest row: the label and key columns plus the
// description column, and the width of a row that draws with no description
// column at all, the widths a box keeps when nothing may trim.
func listNeedW(all []listItem) (labelW, keyW, descNeeds, rowW int) {
	for _, it := range all {
		w := ansi.StringWidth(it.mark.Text)
		if it.segs != nil {
			w += hostapi.SegsWidth(it.segs)
		} else {
			w += ansi.StringWidth(it.label)
		}
		if it.desc != "" { // a row with no description needs no column
			labelW = max(labelW, w)
			descNeeds = max(descNeeds, ansi.StringWidth(it.desc))
		}
		rowW = max(rowW, w+2) // the row's marker rides in front
		if it.key.Text != "" {
			keyW = max(keyW, 2+ansi.StringWidth(it.key.Text))
		}
	}
	return labelW, keyW, descNeeds, rowW
}

func listRowsW(items, all []listItem, sel int, v *viewport, room, minW, maxW int) [][]hostapi.Segment {
	var rows [][]hostapi.Segment
	keys := map[int]hostapi.Segment{}             // row -> key column
	cut, starts := set.Set[int]{}, set.Set[int]{} // rows a scrolled window may end after, and start at
	var selTop, bandTop, selEnd int
	labelW, keyW, descNeeds, _ := listNeedW(all)
	under := false
	if maxW > 0 {
		avail := maxW - 4 - keyW  // the marker and the gap take 2 cells each
		need := max(descNeeds, 3) // a stub desc still draws beside the label
		switch {
		case labelW+need <= avail:
		case avail >= 32:
			// the longer column gives way: labels win while a description is
			// the longer thing, and a description keeps its need otherwise
			if labelW > descNeeds {
				labelW = max(min(labelW, max(avail-need, 20)), 1)
			} else {
				labelW = min(labelW, avail-3)
			}
		default:
			under = true
		}
	}
	section := ""
	for i, it := range items {
		if len(rows) > 0 && (it.section != section || len(it.more) > 0 || len(items[i-1].more) > 0) {
			rows = append(rows, nil)
		}
		start := len(rows)
		starts.Add(start)
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
			kw := 0
			if it.key.Text != "" {
				kw = 2 + ansi.StringWidth(it.key.Text)
			}
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
		cut.Add(len(rows) - 1)
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
		kw := 0
		if ok {
			kw = 2 + ansi.StringWidth(k.Text)
		}
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
	v.reveal(selTop-_boxFade, selEnd+_boxFade)
	for v.top < selTop && !starts.Has(v.top) {
		v.top++
	}
	// start and end the window on whole items, so no item shows cut, no
	// blank separator shows at an edge and no header shows without its
	// first item; an item taller than the window is cut anyway
	end := v.top + v.viewH
	for end > v.top+1 && !cut.Has(end-1) {
		end--
	}
	if !cut.Has(end - 1) {
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
	return v.window(win, widest, _boxFade)
}

// matchSegs draws the runes at byte offsets pos of a row's text, a filter's
// match, before the row's style fills the segments without one: in the
// accent, bold on the selected row.
func matchSegs(segs []hostapi.Segment, pos []int, sel bool) []hostapi.Segment {
	if len(pos) == 0 {
		return segs
	}
	hl := hostapi.ChromeHintKey
	if sel {
		hl = hostapi.ChromeTitle
	}
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
func shimmer(thm themeResolver, frame int, seg hostapi.Segment) []hostapi.Segment {
	dim, fg := thm.style(seg.HL).Fg, thm.style("Normal").Fg
	if dim == nil || fg == nil {
		return []hostapi.Segment{{Text: seg.Text, HL: seg.HL}}
	}
	const band = 3.0
	runes := []rune(seg.Text)
	pos := float64(frame*_frameMS%2000)/2000*(float64(len(runes))+2*band) - band
	out := make([]hostapi.Segment, 0, len(runes))
	for i, r := range runes {
		k := 0.0
		if d := math.Abs(float64(i) - pos); d < band {
			k = math.Round((1+math.Cos(math.Pi*d/band))/2*8) / 8
		}
		cr, cg, cb, _ := hostapi.Mix(dim, fg, k).RGBA()
		hl := hostapi.SGRPrefix + "38;2;" + strconv.Itoa(int(cr>>8)) + ";" + strconv.Itoa(int(cg>>8)) + ";" + strconv.Itoa(int(cb>>8))
		out = append(out, hostapi.Segment{Text: string(r), HL: hl})
	}
	return out
}

// segEffects expands a row's segment effects where a draw applies them: a
// shimmer segment becomes one per rune carrying the band's blend, a spin one
// steps its spinner glyph to frame. It reports whether a segment that moves
// by itself was drawn, so the frame clock keeps stepping while one is on
// screen.
func segEffects(thm themeResolver, frame int, segs []hostapi.Segment) ([]hostapi.Segment, bool) {
	i := slices.IndexFunc(segs, func(s hostapi.Segment) bool { return s.Effect != "" })
	if i < 0 {
		return segs, false
	}
	out := append([]hostapi.Segment(nil), segs[:i]...)
	spun := false
	for _, s := range segs[i:] {
		switch s.Effect {
		case hostapi.EffectShimmer:
			for _, r := range shimmer(thm, frame, s) {
				r.Over, r.Fill, r.Fade, r.OnPanel = s.Over, s.Fill, s.Fade, s.OnPanel
				out = append(out, r)
			}
			spun = spun || frame != 0
		case hostapi.EffectSpin:
			if _, n := utf8.DecodeRuneInString(s.Text); n > 0 {
				s.Text = hostapi.Spinner[frame%len(hostapi.Spinner)] + s.Text[n:]
			}
			out = append(out, s)
			spun = spun || frame != 0
		default:
			out = append(out, s)
		}
	}
	return out, spun
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
		// the facts row already carries a one-line command: the panel below
		// would repeat it and eat a fifth of a short screen
		if strings.Count(req.Arg, "\n") == 0 && ansi.StringWidth(sanitize.String(req.Arg)) <= inner {
			break
		}
		content = a.codePanel(req.Arg, "bash", hostapi.FillCanvas, inner)
	case isDiff:
		content = hostapi.DiffRows(req.Preview, inner)
		for _, r := range content {
			for i := range r {
				r[i].HL = hostapi.ChromePrefix + r[i].HL // the chrome's Diff* styles
			}
		}
	case req.Preview != "":
		content = a.codePanel(req.Preview, previewLang(req), hostapi.FillCanvas, inner)
	}
	var btns []promptChoice
	for _, c := range pa.choices() {
		btns = append(btns, c.promptChoice)
	}
	hints := []hintPair{_pickHint, {key: "esc", verb: "deny"}}
	placeholder := _oneRow
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
		room := h - 2 - len(stack(tight, placeholder, list, _oneRow)) + len(placeholder)
		left := func() int {
			facts := len(target) + nWhy + nSays
			gap := 0
			if facts > 0 && !tight {
				gap = 1
			}
			return room - facts - gap
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
		hints = append(hints, _scrollHint)
	}
	hintRow := hint(inner, hints...)
	// panels span the box, and the box fits its content: a padded panel
	// counts by its text, other rows whole as hostBox counts them
	fit := _listBoxW
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
	content = pa.view.window(content, fit, _boxFade)
	// who asks, whose code runs, and how many asks wait (an upper bound: a
	// waiting ask may settle without a prompt)
	badge := []string{req.Agent, req.Script}
	if n := a.host.AsksWaiting(); n > 1 {
		badge = append(badge, fmt.Sprintf("1 of %d", n))
	}
	body := stack(tight, facts, content, list, [][]hostapi.Segment{hintRow})
	// the title says where a yes runs, so the badge gets what it leaves, a
	// third of the box at least
	title, bw := permTitle(req), min(w-4, maxW)
	joined := strings.Join(slices.DeleteFunc(badge, func(s string) bool { return s == "" }), "   ")
	who := ansi.Truncate(joined, max(bw-ansi.StringWidth(title)-7, bw/3), "…")
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

// permTitle is the question a request asks, from its typed fields.
func permTitle(req hostapi.PermRequest) string {
	switch server, mcp := strings.CutPrefix(req.Subject, "mcp."); {
	case req.Subject == perm.SubjectShell:
		return "Run this command?"
	case req.Subject == perm.SubjectRead && req.Tool == string(tools.ToolSkill):
		return "Load this skill?"
	case req.Subject == perm.SubjectRead:
		return "Read this file?"
	case req.Tool == string(tools.ToolWrite):
		return "Write this file?"
	case req.Tool == string(tools.ToolEdit):
		return "Edit this file?"
	case req.Subject == perm.SubjectWrite:
		return "Change this file?"
	case mcp:
		return "Use " + req.Arg + " from " + server + "?"
	case req.Subject == perm.SubjectLua:
		return "Run Lua inside zemi?"
	case req.Subject == perm.SubjectWorkflow:
		return "Run this workflow?"
	case req.Subject == "tool."+req.Tool:
		return "Use the " + req.Tool + " tool?"
	default:
		return "Allow " + req.Subject + "?"
	}
}

// permAlert is the one line a permission ask pings with: the ask's label and
// the target it acts on, as the status record states it.
func permAlert(req hostapi.PermRequest) string {
	return "zemi: " + lowerFirst(ansi.Truncate(sanitize.Line(askStatus(req)), 200, "…"))
}

// lowerFirst reads s inside the "zemi: " label, where the asks read
// lowercase; permTitle stays capitalized as the overlay box title.
func lowerFirst(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 {
		return s
	}
	return string(unicode.ToLower(r)) + s[size:]
}

// permFacts is the facts table under the prompt's title, three blocks
// sharing one name column: the target (the command a call runs, a file,
// a URL), why zemi asked, and the model's own words, quoted and last since
// nothing checked them. why and says take two rows at most, a target a
// third of the screen, keeping its first and last rows.
func permFacts(req hostapi.PermRequest, root string, w, h int) (target, why, says [][]hostapi.Segment) {
	name, val := permTarget(req, root)
	kw := 0
	for n, on := range map[string]bool{name: val != nil, "why": req.Reason != "", "about": req.Description != ""} {
		if on {
			kw = max(kw, ansi.StringWidth(n)+2)
		}
	}
	if val != nil {
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
		says = capRows(field("about", kw, []hostapi.Segment{{Text: `"` + sanitize.Line(req.Description) + `"`, HL: hostapi.ChromeHint}}, w), 2, w)
	}
	return target, why, says
}

// permTarget is the facts table's first row: what a request acts on, as
// its name and value; no value when the title names it (an mcp tool, a script).
func permTarget(req hostapi.PermRequest, root string) (string, []hostapi.Segment) {
	// a path, URL or command wraps at its slashes: its file name, its host
	// and its pipes are what the user approves
	parts := func(s string) []hostapi.Segment {
		var segs []hostapi.Segment
		for p := range strings.SplitAfterSeq(s, "/") {
			segs = append(segs, hostapi.Segment{Text: p, HL: hostapi.ChromeValue})
		}
		return segs
	}
	switch arg := req.Arg; {
	case req.Subject == perm.SubjectShell:
		return "command", parts(arg)
	case tools.PathSubject(req.Subject), req.Subject == perm.SubjectLua && arg != "script":
		if rel, ok := perm.Under(root, arg); ok && root != "" && filepath.IsAbs(arg) {
			arg = rel
		}
		return "file", parts(arg)
	case req.Subject == perm.SubjectWorkflow && arg != "script":
		return "workflow", []hostapi.Segment{{Text: arg, HL: hostapi.ChromeValue}}
	case strings.HasPrefix(req.Subject, "mcp."), req.Subject == "tool."+req.Tool, arg == "script":
		return "", nil
	default:
		return "arg", parts(req.Arg)
	}
}

// hintPair is one key of a footer hint and the verb it does; row marks the
// selected list row's own keys, which a narrow footer keeps over the
// screen's state keys.
type hintPair struct {
	key, verb string
	row       bool
}

// enter's hint in every prompt, in the lists' words (enterVerb): one verb
// for every button and one for typed text, as wide as "pick" and "open",
// since a changing width shifts the row.
var (
	_pickHint = hintPair{key: "enter", verb: "pick"}
	_sendHint = hintPair{key: "enter", verb: "send"}
)

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
	case p == _scrollHint || p == _scrollGap:
		return 6
	default:
		return 4
	}
}

// hint is the one footer row every box draws: "enter pick   esc deny",
// the pairs in slot order. Past w cells the middle pairs give way,
// scrolling and copy first, then the screen's state keys last first, and a
// row's own key only once no state key is left; enter and esc never do.
func hint(w int, pairs ...hintPair) []hostapi.Segment {
	pairs = slices.Clone(pairs)
	slices.SortStableFunc(pairs, func(a, b hintPair) int { return hintSlot(a) - hintSlot(b) })
	for {
		var out []hostapi.Segment
		for _, p := range pairs {
			switch {
			case p == _scrollGap && len(out) > 0: // scrollHint's cells, its separator included
				out = append(out, hostapi.Segment{Text: strings.Repeat(" ", 4+ansi.StringWidth(_scrollHint.key+_scrollHint.verb))})
				continue
			case p == _scrollGap:
				continue
			case len(out) > 0:
				out = append(out, hostapi.Segment{Text: "   ", HL: hostapi.ChromeHint})
			}
			out = append(out, hostapi.Segment{Text: p.key, HL: hostapi.ChromeHintKey})
			if p.verb != "" {
				out = append(out, hostapi.Segment{Text: " " + p.verb, HL: hostapi.ChromeHint})
			}
		}
		i := slices.IndexFunc(pairs, func(p hintPair) bool { return p == _scrollHint || p == _scrollGap })
		if i < 0 {
			i = slices.IndexFunc(pairs, func(p hintPair) bool { return p.key == "y" })
		}
		if i < 0 {
			// the state keys give way last first, a row's own key only once no
			// state key is left; enter and esc never do
			scan := func(row bool) int {
				i := len(pairs) - 1
				for i >= 0 && (pairs[i].key == "enter" || pairs[i].key == "esc" || pairs[i].row != row) {
					i--
				}
				return i
			}
			if i = scan(false); i < 0 {
				i = scan(true)
			}
		}
		if hostapi.SegsWidth(out) <= w || i < 0 {
			return out
		}
		pairs = slices.Delete(pairs, i, i+1)
	}
}

// _scrollHint names the half-page keys, which every box's text scrolls on
// and a laptop has.
var _scrollHint = hintPair{key: "ctrl+d/u", verb: "scroll"}

// _scrollGap keeps scrollHint's cells blank in a list that can grow past its
// view, so the hint coming moves none of the keys after it.
var _scrollGap = hintPair{verb: "scroll gap"}

// scrolls is scrollHint while any of views overflows.
func scrolls(views ...viewport) []hintPair {
	for _, v := range views {
		if v.overflows() {
			return []hintPair{_scrollHint}
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
	default:
		return ""
	}
}

// codePanel shows text in a well of the canvas inside the box, wrapped to
// width and highlighted as lang when it names one, with a row of canvas
// above and below so the well never touches what surrounds it.
func (a *App) codePanel(text, lang string, fill hostapi.Fill, width int) [][]hostapi.Segment {
	rows := a.wellCode(text, lang, fill, width)
	return append(append([][]hostapi.Segment{wellPad(fill, width)}, rows...), wellPad(fill, width))
}

// wellCode is code's rows in a well's side padding, anything the language
// leaves plain read as a value.
func (a *App) wellCode(text, lang string, fill hostapi.Fill, width int) [][]hostapi.Segment {
	rows := wrapText(text, "", width-2)
	if lang != "" {
		rows = a.host.Highlight(text, lang, width-2)
	}
	for _, r := range rows {
		for i := range r {
			r[i].HL = cmp.Or(r[i].HL, hostapi.ChromeValue)
		}
	}
	return wellRows(rows, fill, width)
}

// wellRow is one row of a well at width cells: its surface runs to both edges.
func wellRow(segs []hostapi.Segment, fill hostapi.Fill, width int) []hostapi.Segment {
	row := make([]hostapi.Segment, 0, len(segs)+2)
	row = append(row, hostapi.Segment{Text: " ", Fill: fill})
	used := 0
	for _, sg := range segs {
		used += ansi.StringWidth(sg.Text)
		sg.Fill = fill
		row = append(row, sg)
	}
	return append(row, hostapi.Segment{Text: strings.Repeat(" ", max(0, width-2-used)) + " ", Fill: fill})
}

// wellPad is one row of a well's surface alone.
func wellPad(fill hostapi.Fill, width int) []hostapi.Segment {
	return []hostapi.Segment{{Text: strings.Repeat(" ", width), Fill: fill}}
}
