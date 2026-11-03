package ui

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/hostapi"
)

// composerState is the multiline input editor (DESIGN §8): text with a
// rune cursor, visual wrapping, and submitted-message history.
type composerState struct {
	text   string
	cursor int               // rune index, 0 <= cursor <= len(runes)
	pastes map[string]string // paste token -> the text it stands for
	pasteN int               // tokens handed out, so a number is never reused
	secret bool              // an input overlay's secret: a bullet per rune, the text never echoes

	history []string
	histIdx int // len(history) = live draft
	draft   string

	undo   []composerSnap // states before edits, for ctrl+z
	typing bool           // the last key typed a word character
}

// maxDraftChars caps the draft's literal text: every keystroke rewraps and
// recomposes the whole draft, so a huge one outruns the key loop and input
// stops echoing, and the send boundary refuses the message anyway. Bigger
// text rides a paste token, which expand unfolds at submit.
const maxDraftChars = 20_000

type composerSnap struct {
	text   string
	cursor int
}

// snapshot records the state before a key for ctrl+z when the key changed
// the text; a run of typed word characters is one step.
func (c *composerState) snapshot(prev composerSnap, word bool) {
	if c.text != prev.text && !(word && c.typing) {
		c.undo = append(c.undo, prev)
		if len(c.undo) > 100 {
			c.undo = c.undo[1:]
		}
	}
	c.typing = word
}

func (c *composerState) undoLast() {
	if n := len(c.undo); n > 0 {
		c.text, c.cursor = c.undo[n-1].text, c.undo[n-1].cursor
		c.undo, c.typing = c.undo[:n-1], false
	}
}

func (c *composerState) runes() []rune { return []rune(c.text) }

func (c *composerState) empty() bool { return strings.TrimSpace(c.text) == "" }

// insert places s at the cursor and moves past it.
func (c *composerState) insert(s string) { c.replace(c.cursor, s) }

// pasteToken matches a paste token ending at the cursor.
var pasteToken = regexp.MustCompile(`\[Pasted text #\d+ [^\]]+\]$`)

// insertFits inserts s at the cursor unless it would push the draft past
// maxDraftChars; ok false leaves the draft alone.
func (c *composerState) insertFits(s string) bool {
	if utf8.RuneCountInString(c.text)+utf8.RuneCountInString(s) > maxDraftChars {
		return false
	}
	c.insert(s)
	return true
}

// paste inserts s, folding a large paste (over 10 lines or 1000 chars)
// into a "[Pasted text #N +L lines]" token that expand restores on submit.
// ok false when the draft cap refused the text.
func (c *composerState) paste(s string) bool {
	lines := strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
	if lines <= 10 && len(s) <= 1000 {
		return c.insertFits(s)
	}
	if c.pastes == nil {
		c.pastes = map[string]string{}
	}
	c.prunePastes()
	size := fmt.Sprintf("+%d lines", lines)
	if lines == 1 {
		size = fmt.Sprintf("%d chars", len([]rune(s)))
	}
	c.pasteN++
	token := fmt.Sprintf("[Pasted text #%d %s]", c.pasteN, size)
	c.pastes[token] = s
	c.insert(token)
	return true
}

// prunePastes drops the text of tokens nothing can name any more: the
// draft and the undo stack are all that can bring one back.
func (c *composerState) prunePastes() {
	maps.DeleteFunc(c.pastes, func(token, _ string) bool {
		return !strings.Contains(c.text, token) &&
			!slices.ContainsFunc(c.undo, func(u composerSnap) bool { return strings.Contains(u.text, token) })
	})
}

// expand swaps paste tokens in text for what was pasted, in one pass, so a
// token-like string inside a paste stays as it is.
func (c *composerState) expand(text string) string {
	pairs := make([]string, 0, 2*len(c.pastes))
	for token, s := range c.pastes {
		pairs = append(pairs, token, s)
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

// isMark reports a combining mark: it extends the rune before it into one
// cluster. NOTE: base+marks only, not full grapheme handling (no ZWJ, no
// regional indicators).
func isMark(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Mc)
}

// backspace deletes the cluster before the cursor - a base with the marks
// that render on it - or a whole paste token.
func (c *composerState) backspace() {
	if c.cursor == 0 {
		return
	}
	r := c.runes()
	if tok := pasteToken.FindString(string(r[:c.cursor])); c.pastes[tok] != "" {
		c.replace(c.cursor-len([]rune(tok)), "")
		return
	}
	i := c.cursor
	for i > 0 && isMark(r[i-1]) {
		i--
	}
	if i > 0 {
		i-- // the base the marks sit on
	}
	c.replace(i, "")
}

// delete removes the cluster at the cursor: a base with the marks that
// follow it.
func (c *composerState) delete() {
	r := c.runes()
	if c.cursor >= len(r) {
		return
	}
	end := c.cursor + 1
	for end < len(r) && isMark(r[end]) {
		end++
	}
	c.text = string(r[:c.cursor]) + string(r[end:])
}

func (c *composerState) left() {
	if c.cursor > 0 {
		c.cursor--
	}
}

func (c *composerState) right() {
	if c.cursor < len(c.runes()) {
		c.cursor++
	}
}

// lineSpan returns the rune offsets [start, end) of the line holding the
// cursor.
func (c *composerState) lineSpan() (int, int) {
	r := c.runes()
	start := c.cursor
	for start > 0 && r[start-1] != '\n' {
		start--
	}
	end := c.cursor
	for end < len(r) && r[end] != '\n' {
		end++
	}
	return start, end
}

// rowStarts maps each visual row to the rune index it starts at: the rows
// are contiguous except across a hard break, which skips its '\n'.
func rowStarts(rows [][]rune, soft []bool) []int {
	out := make([]int, len(rows))
	at := 0
	for i, r := range rows {
		out[i] = at
		at += len(r)
		if !soft[i] {
			at++
		}
	}
	return out
}

// composerMove moves the cursor one visual row up (-1) or down (1),
// keeping the column; false on the first or last row, where up and down
// recall history instead.
func (a *App) composerMove(delta int) bool {
	glyph, _, _ := a.draftMode()
	row, col, rows, soft := a.wrapComposer(draftWrap(a.composerW, glyph))
	to := row + delta
	if to < 0 || to >= len(rows) {
		return false
	}
	a.composer.cursor = rowStarts(rows, soft)[to] + min(col, len(rows[to]))
	return true
}

// isEditKey reports whether key moves or edits a typed line, without
// applying it.
func isEditKey(key string) bool {
	switch key {
	case "backspace", "delete", "left", "right", "alt+left", "alt+b", "ctrl+left",
		"alt+right", "alt+f", "ctrl+right", "home", "ctrl+a", "end", "ctrl+e",
		"ctrl+w", "alt+backspace", "ctrl+u":
		return true
	default:
		return false
	}
}

// editKey applies a cursor move or deletion key; it reports whether key was
// one. ctrl+u kills the line's text back to its start - a prompt's or a
// filter's; the composer's own draft keeps it for the half-page scroll.
func (c *composerState) editKey(key string) bool {
	if !isEditKey(key) {
		return false
	}
	switch key {
	case "backspace":
		c.backspace()
	case "delete":
		c.delete()
	case "left":
		c.left()
	case "right":
		c.right()
	case "alt+left", "alt+b", "ctrl+left":
		c.wordLeft()
	case "alt+right", "alt+f", "ctrl+right":
		c.wordRight()
	case "home", "ctrl+a":
		c.lineStart()
	case "end", "ctrl+e":
		c.lineEnd()
	case "ctrl+w", "alt+backspace":
		c.delWord()
	case "ctrl+u":
		// kill to the line's start, readline's: a prefilled prompt's value
		// clears instead of taking typed text as an append
		r := c.runes()
		start := c.cursor
		for start > 0 && r[start-1] != '\n' {
			start--
		}
		c.replace(start, "")
	}
	return true
}

// typeKey applies an editing key, else types text: a one-line field's keys.
func (c *composerState) typeKey(key, text string) {
	if !c.editKey(key) {
		c.insert(text)
	}
}

// replace swaps the runes from start to the cursor for s.
func (c *composerState) replace(start int, s string) {
	r := c.runes()
	c.text = string(r[:start]) + s + string(r[c.cursor:])
	c.cursor = start + len([]rune(s))
}

func (c *composerState) wordLeft() {
	r, i := c.runes(), c.cursor
	for i > 0 && unicode.IsSpace(r[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(r[i-1]) {
		i--
	}
	c.cursor = i
}

func (c *composerState) wordRight() {
	r, i := c.runes(), c.cursor
	for i < len(r) && unicode.IsSpace(r[i]) {
		i++
	}
	for i < len(r) && !unicode.IsSpace(r[i]) {
		i++
	}
	c.cursor = i
}

func (c *composerState) lineStart() { c.cursor, _ = c.lineSpan() }
func (c *composerState) lineEnd()   { _, c.cursor = c.lineSpan() }

// delWord removes the word before the cursor (ctrl+w): a run of spaces
// then a run of non-space, with '\n' a hard boundary.
func (c *composerState) delWord() {
	r := c.runes()
	i := c.cursor
	for i > 0 && (r[i-1] == ' ' || r[i-1] == '\t') {
		i--
	}
	if i > 0 && r[i-1] == '\n' {
		i--
	} else {
		for i > 0 && !strings.ContainsRune(" \t\n", r[i-1]) {
			i--
		}
	}
	c.replace(i, "")
}

func (c *composerState) moveEnd() { c.cursor = len(c.runes()) }

// The history walk: histIdx runs 0..len(history), len(history) the live
// draft - ctrl+p walks back to the entries, ctrl+n returns to the draft.
const historyCap = 50

func (c *composerState) pushHistory(text string) {
	// a repeat moves the entry to the end, wherever it was
	c.history = slices.DeleteFunc(c.history, func(s string) bool { return s == text })
	c.history = append(c.history, text)
	if len(c.history) > historyCap {
		c.history = c.history[len(c.history)-historyCap:]
	}
	c.histIdx = len(c.history)
}

// seedHistory adds the project's recently typed messages to the history,
// so a restart or a resume does not lose them. It runs once, when a recall
// starts.
// NOTE: reads them on the key loop; one capped query is small enough
// (upgrade: load it in the background at boot).
func (a *App) seedHistory() {
	if a.histLoaded {
		return
	}
	a.histLoaded = true
	stored := a.host.InputHistory(historyCap)
	c := &a.composer
	// what this run typed is stored too; keep one copy, in store order
	c.history = slices.DeleteFunc(c.history, func(s string) bool { return slices.Contains(stored, s) })
	c.history = append(c.history, stored...)
	if len(c.history) > historyCap {
		c.history = c.history[len(c.history)-historyCap:]
	}
	c.histIdx = len(c.history)
}

// histPrev recalls the older entry, saving the live draft.
func (c *composerState) histPrev() {
	if c.histIdx == 0 {
		return
	}
	if c.histIdx == len(c.history) {
		c.draft = c.text
	}
	c.histIdx--
	c.text = c.history[c.histIdx]
	c.moveEnd()
}

// histNext recalls the newer entry or restores the draft.
func (c *composerState) histNext() {
	if c.histIdx >= len(c.history) {
		return
	}
	c.histIdx++
	if c.histIdx == len(c.history) {
		c.text = c.draft
	} else {
		c.text = c.history[c.histIdx]
	}
	c.moveEnd()
}

const composerMaxRows = 8

// draftWrap is the cells a draft row wraps at on w: one kept free for the
// cursor mark, two more where the slash glyph opens the gutter.
func draftWrap(w int, glyph string) int {
	if glyph != "" {
		return w - 3
	}
	return w - 1
}

// composerRows renders the composer as visual rows wrapped to width w: at
// most composerMaxRows rows, scrolled to keep the cursor row in view with a
// row of headroom below it, the rows past either edge fading toward the
// panel. A plain draft starts at the frame's inner edge; a slash (/) draft
// keeps its glyph on the first row and the gutter it opens down the
// others. An empty draft shows placeholder dim under the cursor, where typing
// replaces it without moving a cell.
func (a *App) composerRows(w int, placeholder string) [][]hostapi.Segment {
	glyph, promptHL, _ := a.draftMode()
	wrap := draftWrap(w, glyph)
	a.composerW = w
	cursorRow, cursorCol, rows, soft := a.wrapComposer(wrap)
	start := max(0, cursorRow-composerMaxRows+1)
	if cursorRow < len(rows)-1 {
		// a row of headroom, so the bottom ramp lands below the live cursor
		start = max(0, cursorRow-composerMaxRows+2)
	}
	focused := a.topChrome() != noModal || a.chatFocus // the chat cursor takes every key: the composer draws no cursor
	var out [][]hostapi.Segment
	for i := start; i < len(rows) && len(out) < composerMaxRows; i++ {
		text, col := rows[i], cursorCol
		var gutter []hostapi.Segment
		switch {
		case glyph == "":
		case i == 0:
			// the glyph stands for the draft's own first character, so the row leaves it out
			text, col = text[1:], max(col-1, 0)
			gutter = []hostapi.Segment{{Text: glyph + " ", HL: promptHL, NoCopy: true}}
		default:
			gutter = []hostapi.Segment{{Text: "  ", NoCopy: true}}
		}
		var segs []hostapi.Segment
		switch {
		case a.composer.text == "" && placeholder != "":
			if a.composerNote != "" {
				placeholder = a.composerNote
			}
			hint := []hostapi.Segment{{Text: placeholder, HL: "Dim"}}
			if !focused {
				hint = cursorSegs([]rune(placeholder), 0, "Dim")
			}
			for j := range hint {
				hint[j].NoCopy = true
			}
			segs = truncateSegs(hint, max(wrap, 1)) // a row never comes out empty
		case i == cursorRow && !focused:
			segs = cursorSegs(text, min(col, len(text)), "Normal")
		default:
			segs = []hostapi.Segment{{Text: string(text), HL: "Normal"}}
		}
		if len(gutter) > 0 {
			segs = append(gutter, segs...)
		}
		segs[len(segs)-1].Soft = soft[i] // a wrapped row copies as one line
		out = append(out, segs)
	}
	edgeRamp(out, start, len(rows)-start-len(out), boxFade, func(i int) bool { return i == cursorRow-start })
	return out
}

// draftMode is what a draft's first character turns the composer into: a "/"
// runs a slash command; it never reaches the model.
func (a *App) draftMode() (glyph, promptHL, ruleHL string) {
	if strings.HasPrefix(a.composer.text, "/") {
		return "/", "PromptCommand", "RuleCommand"
	}
	return "", "", ""
}

// frameInner is the cells a row inside the composer frame may use: the two
// border columns and a space beside each.
func frameInner(w int) int { return max(w-4, 1) }

// composerFrame draws the composer in a square frame whose top rule carries
// the node's top region: the mascot and the turn's status. Agent state
// changes what the rule says and never how many rows the frame takes, so
// nothing above it reflows.
func (a *App) composerFrame(n *node, w int) [][]hostapi.Segment {
	hl := "Rule"
	if _, _, rule := a.draftMode(); rule != "" {
		hl = rule // a command draft tints the whole frame, not just its glyph
	}
	inner := frameInner(w)
	// the top region draws its rule corner to corner; its "Rule" cells take
	// the frame's tint, and its plain rule runs join what stands on them
	top := truncateSegs(joinItems(a.statusItems(n.Top), false), w-2)
	for i := range top {
		if top[i].HL == "Rule" {
			top[i].HL = hl
			top[i].Line = strings.Trim(top[i].Text, "─") == ""
		}
	}
	rows := [][]hostapi.Segment{rule("┌", "┐", hl, top, w)}
	for _, r := range a.composerRows(inner, n.Placeholder) {
		row := append([]hostapi.Segment{{Text: "│ ", HL: hl, NoCopy: true, Line: true}}, truncateSegs(r, inner)...)
		rows = append(rows, append(padSegs(row, w-2), hostapi.Segment{Text: " │", HL: hl, NoCopy: true, Line: true}))
	}
	return onPanel(append(rows, rule("└", "┘", hl, nil, w)))
}

// rule draws one side of the composer frame across w cells: the corners, the
// cells in the middle, "─" around them.
func rule(l, r, hl string, mid []hostapi.Segment, w int) []hostapi.Segment {
	fill := max(w-2-hostapi.SegsWidth(mid), 0)
	out := append([]hostapi.Segment{{Text: l, HL: hl, NoCopy: true, Line: true}}, mid...)
	out = append(out, hostapi.Segment{Text: strings.Repeat("─", fill), HL: hl, NoCopy: true, Line: true})
	return append(out, hostapi.Segment{Text: r, HL: hl, NoCopy: true, Line: true})
}

// inputRow is a one-line text field at least 40 cells wide: "> " and the
// text with its cursor, scrolled so the cursor stays inside width cells and
// cut to them, so typing never widens the box drawn around it. An empty
// line shows ghost dim under the cursor, what submitting it would mean.
func (c *composerState) inputRow(width int, ghost string) []hostapi.Segment {
	r := []rune(c.text)
	if c.secret {
		for i := range r {
			r[i] = '•'
		}
	}
	segs := cursorSegs(r, c.cursor, "")
	if c.text == "" && ghost != "" {
		segs = cursorSegs([]rune(ghost), 0, "Dim")
	}
	if over := 2 + hostapi.SegsWidth(segs[:2]) - (width - 1); over > 0 {
		segs = dropCells(segs, over)
	}
	return padSegs(ellipsizeSegs(append([]hostapi.Segment{{Text: "> ", HL: "Dim", NoCopy: true}}, segs...), width), 40)
}

// cursorSegs draws r with a block cursor on the rune at cursor, or on a
// blank cell past the end.
func cursorSegs(r []rune, cursor int, hl string) []hostapi.Segment {
	under, rest := " ", ""
	if cursor < len(r) {
		under, rest = string(r[cursor]), string(r[cursor+1:])
	}
	return []hostapi.Segment{{Text: string(r[:cursor]), HL: hl}, {Text: under, HL: hostapi.ChromeCursor}, {Text: rest, HL: hl}}
}

// wrapComposer splits the text into visual rows of at most w cells, on
// newlines and after the last space that fits (a word longer than a row is
// split), and reports the cursor's row and rune column plus which rows wrap
// onto the next rather than ending at a newline.
func (a *App) wrapComposer(w int) (cursorRow, cursorCol int, rows [][]rune, soft []bool) {
	w = max(w, 1)
	if k := &a.wrapKey; k.text == a.composer.text && k.cursor == a.composer.cursor && k.w == w {
		return k.row, k.col, k.rows, k.soft
	}
	r := []rune(a.composer.text)
	for i, x := range r {
		if x == '\t' { // a tab has no cell width; one cell keeps runes and cells aligned
			r[i] = ' '
		}
	}
	type span struct{ s, e int }
	var spans []span
	start, width, brk := 0, 0, 0 // brk: the rune after the row's last space
	for i, x := range r {
		if x == '\n' {
			spans = append(spans, span{start, i})
			start, width, brk = i+1, 0, 0
			continue
		}
		cw := 1
		if x < ' ' || x > '~' { // the ASCII glyphs are one cell, the rest measure
			cw = ansi.StringWidth(string(x))
		}
		if width+cw > w && i > start {
			end := i
			if brk > start {
				end = brk
			}
			spans = append(spans, span{start, end})
			start, width, brk = end, ansi.StringWidth(string(r[end:i])), 0
		}
		width += cw
		if x == ' ' {
			brk = i + 1
		}
	}
	spans = append(spans, span{start, len(r)})
	cur := a.composer.cursor
	for n, sp := range spans {
		// at a soft wrap the cursor belongs to the next row, at a newline to this one
		if cur >= sp.s && (cur < sp.e || cur == sp.e && (n == len(spans)-1 || spans[n+1].s > sp.e)) {
			cursorRow, cursorCol = n, cur-sp.s
		}
		rows = append(rows, r[sp.s:sp.e])
		soft = append(soft, n < len(spans)-1 && spans[n+1].s == sp.e)
	}
	a.wrapKey = struct {
		text      string
		cursor, w int
		row, col  int
		rows      [][]rune
		soft      []bool
	}{a.composer.text, a.composer.cursor, w, cursorRow, cursorCol, rows, soft}
	return cursorRow, cursorCol, rows, soft
}

// editorMsg carries the draft an external editor saved.
type editorMsg struct {
	text string
	err  error
}

// editInEditor opens text in the user's editor (ctrl+g on the draft, e on a
// focused chat message) on a temp file removed when the editor exits.
// readBack hands the saved text to the composer; e on the chat reads
// nothing back — NOTE: its edits are discarded, it is a copying aid.
func (a *App) editInEditor(text, title string, readBack bool) tea.Cmd {
	fail := func(err error) tea.Cmd {
		a.pushToast(slog.LevelError, "Could not open the editor", err.Error())
		return nil
	}
	dir, err := a.host.TempDir()
	if err != nil {
		return fail(err)
	}
	f, err := os.CreateTemp(dir, "zemi-*.md")
	if err != nil {
		return fail(err)
	}
	path := f.Name()
	_, err = f.WriteString(text)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return fail(err)
	}
	a.host.GoSafe("edit "+title, func() {
		defer os.Remove(path)
		var msg editorMsg
		if msg.err = a.EditFile(context.Background(), hostapi.EditRequest{Path: path, Title: title}); msg.err == nil && readBack {
			b, err := os.ReadFile(path)
			msg.text, msg.err = strings.TrimRight(string(b), "\n"), err
		}
		// await, not dispatch: a full queue must not drop the saved draft
		a.await(func() tea.Cmd { _, cmd := a.Update(msg); return cmd })
	})
	return nil
}

// openEditor edits the draft in the user's editor (ctrl+g).
func (a *App) openEditor() tea.Cmd {
	return a.editInEditor(a.composer.expand(a.composer.text), "the draft", true)
}
