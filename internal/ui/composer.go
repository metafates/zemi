package ui

import (
	"strings"

	"github.com/metafates/zemi/internal/hostapi"
)

// composerState is the multiline input editor (DESIGN §8): text with a
// rune cursor, visual wrapping, and submitted-message history.
type composerState struct {
	text   string
	cursor int // rune index, 0 <= cursor <= len(runes)

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

func (c *composerState) backspace() {
	if c.cursor == 0 {
		return
	}
	r := c.runes()
	c.text = string(r[:c.cursor-1]) + string(r[c.cursor:])
	c.cursor--
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
	r := c.runes()
	lineStart := 0
	for i := c.cursor - 1; i >= 0; i-- {
		if r[i] == '\n' {
			lineStart = i + 1
			break
		}
	}
	if lineStart == 0 {
		return false
	}
	prevEnd := lineStart - 1
	prevStart := 0
	for i := prevEnd - 1; i >= 0; i-- {
		if r[i] == '\n' {
			prevStart = i + 1
			break
		}
	}
	col := c.cursor - lineStart
	target := prevStart + col
	if target > prevEnd {
		target = prevEnd
	}
	c.cursor = target
	return true
}

// down moves the cursor one line down, keeping the column; reports whether
// it moved.
func (c *composerState) down() bool {
	r := c.runes()
	lineStart := 0
	for i := c.cursor - 1; i >= 0; i-- {
		if r[i] == '\n' {
			lineStart = i + 1
			break
		}
	}
	nextStart := -1
	for i := lineStart; i < len(r); i++ {
		if r[i] == '\n' {
			nextStart = i + 1
			break
		}
	}
	if nextStart < 0 {
		return false
	}
	col := c.cursor - lineStart
	nextEnd := len(r)
	for i := nextStart; i < len(r); i++ {
		if r[i] == '\n' {
			nextEnd = i
			break
		}
	}
	target := nextStart + col
	if target > nextEnd {
		target = nextEnd
	}
	c.cursor = target
	return true
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
	cursorRow, rows := a.wrapComposer(w-len(prefix))
	if len(rows) == 0 {
		rows = [][]rune{nil}
		cursorRow = 0
	}
	start := 0
	if len(rows) > composerMaxRows {
		start = cursorRow - composerMaxRows + 1
		if start < 0 {
			start = 0
		}
	}
	// The prompt takes the permission mode's color.
	promptHL := map[string]string{"auto": "PromptAuto", "yolo": "PromptYolo"}[a.host.PermMode()]
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
		text := string(rows[i])
		marker := "▏"
		if i == cursorRow {
			segs = append(segs,
				hostapi.Segment{Text: text, HL: "Normal"},
				hostapi.Segment{Text: marker, HL: "Comment"},
			)
		} else {
			segs = append(segs, hostapi.Segment{Text: text, HL: "Normal"})
		}
		out = append(out, segs)
	}
	return out
}

// wrapComposer splits the text into visual rows of at most width runes,
// splitting on newlines, and reports which row holds the cursor.
func (a *App) wrapComposer(w int) (cursorRow int, rows [][]rune) {
	if w < 1 {
		w = 1
	}
	r := []rune(a.composer.text)
	cur := []rune{}
	row := 0
	for i, x := range r {
		if i == a.composer.cursor {
			cursorRow = row
		}
		if x == '\n' {
			rows = append(rows, cur)
			cur = nil
			row++
			continue
		}
		cur = append(cur, x)
		if len(cur) == w {
			rows = append(rows, cur)
			cur = nil
			row++
		}
	}
	rows = append(rows, cur)
	if a.composer.cursor >= len(r) {
		cursorRow = row
	}
	return cursorRow, rows
}
