// Package sanitize strips terminal control sequences from model-influenced
// strings at the boundary before they reach widgets or overlays.
package sanitize

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// String removes ANSI escape sequences (CSI, OSC, other C1 controls) and
// collapses C0 controls other than newline and tab, and kitty image
// placeholders, into spaces. Other runes stay, wide ones included.
func String(s string) string {
	if !needsWork(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = skipEscape(s, i)
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1, r < 0x20 && r != '\n' && r != '\t', r >= 0x7f && r < 0xa0, r == 0x10eeee:
			// a kitty image placeholder draws whichever image its color names
			b.WriteByte(' ')
		case unicode.Is(unicode.Bidi_Control, r), r >= 0xe0000 && r <= 0xe007f, r >= 0xfff9 && r <= 0xfffb, r == 0xad:
			// bidi controls reorder text; tag and annotation runes hide it. A soft
			// hyphen measures 0 wide but terminals draw 1 column. Other format
			// runes (ZWJ, ZWNJ) join emoji and shape scripts.
			// NOTE: subdivision flags lose their tags and show as a plain black flag.
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// needsWork is a fast path for plain printable ASCII.
func needsWork(s string) bool {
	for i := range len(s) {
		if c := s[i]; c < 0x20 && c != '\n' && c != '\t' || c >= 0x7f {
			return true
		}
	}
	return false
}

// skipEscape consumes one escape sequence starting at s[i] == 0x1b and
// returns the index just past it.
func skipEscape(s string, i int) int {
	j := i + 1
	if j >= len(s) {
		return j
	}
	switch s[j] {
	case '[': // CSI: parameter, intermediate and final bytes
		for j++; j < len(s); j++ {
			if s[j] >= 0x40 && s[j] <= 0x7e {
				return j + 1
			}
		}
		return j
	case ']', 'P', 'X', '^', '_': // OSC, DCS, SOS, PM, APC: ended by ST (ESC \), OSC by BEL too
		for j++; j < len(s); j++ {
			if s[j] == 0x07 && s[i+1] == ']' {
				return j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
		}
		return j
	default:
		// nF (ESC ( B): intermediates then a final byte; otherwise two bytes
		for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
			j++
		}
		if j < len(s) {
			j++
		}
		return j
	}
}

// Line is String with every run of whitespace collapsed to one space, so
// the text fits one row.
func Line(s string) string { return strings.Join(strings.Fields(String(s)), " ") }
