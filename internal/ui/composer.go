package ui

import (
	"context"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode"

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

	history []string
	histIdx int // len(history) = live draft
	draft   string

	undo   []composerSnap // states before edits, for ctrl+z
	typing bool           // the last key typed a word character
}

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
func (c *composerState) insert(s string) {
	r := c.runes()
	at := c.cursor
	c.text = string(r[:at]) + s + string(r[at:])
	c.cursor = at + len([]rune(s))
}

// pasteToken matches a paste token ending at the cursor.
var pasteToken = regexp.MustCompile(`\[Pasted text #\d+ [^\]]+\]$`)

// paste inserts s, folding a large paste (over 10 lines or 1000 chars)
// into a "[Pasted text #N +L lines]" token that expand restores on submit.
func (c *composerState) paste(s string) {
	lines := strings.Count(s, "\n") + 1
	if lines <= 10 && len(s) <= 1000 {
		c.insert(s)
		return
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

// backspace deletes the rune before the cursor, or a whole paste token.
func (c *composerState) backspace() {
	if c.cursor == 0 {
		return
	}
	r := c.runes()
	n := 1
	if tok := pasteToken.FindString(string(r[:c.cursor])); c.pastes[tok] != "" {
		n = len([]rune(tok))
	}
	c.text = string(r[:c.cursor-n]) + string(r[c.cursor:])
	c.cursor -= n
}

func (c *composerState) delete() {
	r := c.runes()
	if c.cursor >= len(r) {
		return
	}
	c.text = string(r[:c.cursor]) + string(r[c.cursor+1:])
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

// up moves the cursor one line up, keeping the column; reports whether it
// moved.
func (c *composerState) up() bool {
	start, _ := c.lineSpan()
	if start == 0 {
		return false
	}
	col := c.cursor - start
	c.cursor = start - 1
	prevStart, _ := c.lineSpan()
	c.cursor = min(prevStart+col, start-1)
	return true
}

// down moves the cursor one line down, keeping the column; reports whether
// it moved.
func (c *composerState) down() bool {
	start, end := c.lineSpan()
	if end == len(c.runes()) {
		return false
	}
	col := c.cursor - start
	c.cursor = end + 1
	_, nextEnd := c.lineSpan()
	c.cursor = min(end+1+col, nextEnd)
	return true
}

// editKey applies a cursor move or deletion key; it reports whether key was
// one.
func (c *composerState) editKey(key string) bool {
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
	case "ctrl+u":
		c.delToLineStart()
	case "ctrl+k":
		c.delToLineEnd()
	case "ctrl+w", "alt+backspace":
		c.delWord()
	default:
		return false
	}
	return true
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
		for i > 0 && r[i-1] != ' ' && r[i-1] != '\t' && r[i-1] != '\n' {
			i--
		}
	}
	c.text = string(r[:i]) + string(r[c.cursor:])
	c.cursor = i
}

func (c *composerState) delToLineStart() {
	start, _ := c.lineSpan()
	r := c.runes()
	c.text = string(r[:start]) + string(r[c.cursor:])
	c.cursor = start
}

func (c *composerState) delToLineEnd() {
	_, end := c.lineSpan()
	r := c.runes()
	c.text = string(r[:c.cursor]) + string(r[end:])
}

func (c *composerState) moveEnd() {
	c.cursor = len(c.runes())
}

// multiline reports whether newline insertion is allowed.
func (a *App) multiline() bool { return a.host.Settings.Bool("ui.composer.multiline") }

// historySize reads the history cap; 0 disables.
func (a *App) historySize() int { return min(max(a.host.Settings.Int("ui.composer.history"), 0), 1000) }

func (c *composerState) pushHistory(text string, cap int) {
	if cap <= 0 {
		c.history, c.histIdx = nil, 0
		return
	}
	if n := len(c.history); n > 0 && c.history[n-1] == text {
		c.histIdx = n
		return
	}
	c.history = append(c.history, text)
	if len(c.history) > cap {
		c.history = c.history[len(c.history)-cap:]
	}
	c.histIdx = len(c.history)
}

// seedHistory adds the project's recently typed messages to the history,
// so a restart or a resume does not lose them. It runs once, when a recall
// starts.
// NOTE: reads them on the key loop; one capped query is small enough
// (upgrade: load it in the background at boot).
func (a *App) seedHistory() {
	n := a.historySize()
	if a.histLoaded || n <= 0 {
		return
	}
	a.histLoaded = true
	stored := a.host.InputHistory(n)
	c := &a.composer
	// what this run typed is stored too; keep one copy, in store order
	c.history = slices.DeleteFunc(c.history, func(s string) bool { return slices.Contains(stored, s) })
	c.history = append(c.history, stored...)
	if len(c.history) > n {
		c.history = c.history[len(c.history)-n:]
	}
	c.histIdx = len(c.history)
}

// histPrev recalls the older entry, saving the live draft; reports whether
// it moved.
func (c *composerState) histPrev() bool {
	if c.histIdx == 0 {
		return false
	}
	if c.histIdx == len(c.history) {
		c.draft = c.text
	}
	c.histIdx--
	c.text = c.history[c.histIdx]
	c.cursor = len(c.runes())
	return true
}

// histNext recalls the newer entry or restores the draft; reports whether
// it moved.
func (c *composerState) histNext() bool {
	if c.histIdx >= len(c.history) {
		return false
	}
	c.histIdx++
	if c.histIdx == len(c.history) {
		c.text = c.draft
	} else {
		c.text = c.history[c.histIdx]
	}
	c.cursor = len(c.runes())
	return true
}

const composerMaxRows = 8

// composerRows renders the composer as visual rows wrapped to width w:
// "> " on the first row, "  " on continuations, the cursor cell placed in
// the cursor row. At most composerMaxRows rows are shown, scrolled to keep the
// cursor row visible.
func (a *App) composerRows(w int) [][]hostapi.Segment {
	prefix := "> "
	cont := "  "
	// one cell stays free for the cursor mark at the end of a full row
	cursorRow, cursorCol, rows, soft := a.wrapComposer(w - len(prefix) - 1)
	start := max(0, cursorRow-composerMaxRows+1)
	// The prompt takes the permission mode's color.
	promptHL := map[string]string{"plan": "PromptPlan", "auto": "PromptAuto", "yolo": "PromptYolo"}[a.modeBase()]
	if promptHL == "" {
		promptHL = "Comment"
	}
	var out [][]hostapi.Segment
	for i := start; i < len(rows) && len(out) < composerMaxRows; i++ {
		p := cont
		if i == 0 {
			p = prefix
		}
		segs := []hostapi.Segment{{Text: p, HL: promptHL, NoCopy: true}}
		if i == cursorRow && a.focusedNode() == "" { // no cursor while a node has the keys
			segs = append(segs, cursorSegs(rows[i], min(cursorCol, len(rows[i])), "Normal")...)
		} else {
			segs = append(segs, hostapi.Segment{Text: string(rows[i]), HL: "Normal"})
		}
		segs[len(segs)-1].Soft = soft[i] // a wrapped row copies as one line
		out = append(out, segs)
	}
	if hidden := len(rows) - len(out); hidden > 0 {
		out = append(out, []hostapi.Segment{{Text: fmt.Sprintf("  +%d %s", hidden, map[bool]string{true: "line", false: "lines"}[hidden == 1]), HL: "Comment", NoCopy: true}})
	}
	return out
}

// inputRow is a one-line text field at least 40 cells wide: "> " and the
// text with its cursor, scrolled so the cursor stays inside width cells
// (one is left for the ellipsis hostBox puts on a longer row).
func (c *composerState) inputRow(width int) []hostapi.Segment {
	segs := cursorSegs([]rune(c.text), c.cursor, "")
	if over := 2 + segsWidth(segs[:2]) - (width - 1); over > 0 {
		segs = dropCells(segs, over)
	}
	return padSegs(append([]hostapi.Segment{{Text: "> ", HL: "Comment", NoCopy: true}}, segs...), 40)
}

// cursorSegs draws r with a block cursor on the rune at cursor, or on a
// blank cell past the end.
func cursorSegs(r []rune, cursor int, hl string) []hostapi.Segment {
	under, rest := " ", ""
	if cursor < len(r) {
		under, rest = string(r[cursor]), string(r[cursor+1:])
	}
	return []hostapi.Segment{{Text: string(r[:cursor]), HL: hl}, {Text: under, HL: "Host:Cursor"}, {Text: rest, HL: hl}}
}

// wrapComposer splits the text into visual rows of at most w cells, on
// newlines and after the last space that fits (a word longer than a row is
// split), and reports the cursor's row and rune column plus which rows wrap
// onto the next rather than ending at a newline.
func (a *App) wrapComposer(w int) (cursorRow, cursorCol int, rows [][]rune, soft []bool) {
	w = max(w, 1)
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
		cw := ansi.StringWidth(string(x))
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
	return cursorRow, cursorCol, rows, soft
}

// editorMsg carries the draft an external editor saved.
type editorMsg struct {
	text string
	err  error
}

// openEditor edits the draft in the user's editor (ctrl+g) through
// EditFile, off the UI goroutine; the saved text comes back as editorMsg.
func (a *App) openEditor() tea.Cmd {
	for _, wn := range a.host.Wins.List() {
		if wn.Owner == "editor" && wn.Title() == "Edit the draft" && !wn.Exited() {
			a.setFocusWin(wn.ID) // a second editor on the draft would lose one of the two
			return nil
		}
	}
	dir, err := a.host.TempDir()
	if err != nil {
		a.pushToast("error", "Editor failed", err.Error())
		return nil
	}
	f, err := os.CreateTemp(dir, "zemi-*.md")
	if err != nil {
		a.pushToast("error", "Editor failed", err.Error())
		return nil
	}
	path := f.Name()
	_, err = f.WriteString(a.composer.expand(a.composer.text))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		a.pushToast("error", "Editor failed", err.Error())
		return nil
	}
	go func() {
		defer os.Remove(path)
		var msg editorMsg
		if msg.err = a.EditFile(context.Background(), hostapi.EditRequest{Path: path, Title: "the draft"}); msg.err == nil {
			b, err := os.ReadFile(path)
			msg.text, msg.err = strings.TrimRight(string(b), "\n"), err
		}
		// await, not dispatch: a full queue must not drop the saved draft
		a.await(func() tea.Cmd { _, cmd := a.Update(msg); return cmd })
	}()
	return nil
}
