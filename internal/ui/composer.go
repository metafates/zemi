package ui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/metafates/zemi/internal/hostapi"
)

// composerState is the multiline input editor (DESIGN §8): text with a
// rune cursor, visual wrapping, and submitted-message history.
type composerState struct {
	text   string
	cursor int               // rune index, 0 <= cursor <= len(runes)
	pastes map[string]string // paste token -> the text it stands for

	history []string
	histIdx int // len(history) = live draft
	draft   string
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
	size := fmt.Sprintf("+%d lines", lines)
	if lines == 1 {
		size = fmt.Sprintf("%d chars", len([]rune(s)))
	}
	token := fmt.Sprintf("[Pasted text #%d %s]", len(c.pastes)+1, size)
	c.pastes[token] = s
	c.insert(token)
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
func (a *App) multiline() bool { return a.cfgBool("ui.composer.multiline", true) }

// historySize reads the history cap; 0 disables.
func (a *App) historySize() int { return a.cfgInt("ui.composer.history", 50, 0, 1000) }

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
// the cursor row. At most maxRows rows are shown, scrolled to keep the
// cursor row visible.
func (a *App) composerRows(w int) [][]hostapi.Segment {
	prefix := "> "
	cont := "  "
	// one cell stays free for the cursor mark at the end of a full row
	cursorRow, cursorCol, rows := a.wrapComposer(w - len(prefix) - 1)
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
		segs := []hostapi.Segment{{Text: p, HL: promptHL}}
		if i == cursorRow {
			segs = append(segs, cursorSegs(rows[i], min(cursorCol, len(rows[i])), "Normal")...)
		} else {
			segs = append(segs, hostapi.Segment{Text: string(rows[i]), HL: "Normal"})
		}
		out = append(out, segs)
	}
	return out
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
// split), and reports the cursor's row and rune column.
func (a *App) wrapComposer(w int) (cursorRow, cursorCol int, rows [][]rune) {
	w = max(w, 1)
	r := []rune(a.composer.text)
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
	}
	return cursorRow, cursorCol, rows
}
