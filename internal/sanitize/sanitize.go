// Package sanitize strips terminal control sequences from model-influenced
// strings at the boundary before they reach widgets, overlays, or the log.
package sanitize

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// String removes ANSI escape sequences (CSI, OSC, other C1 controls) and
// collapses C0 controls other than newline and tab into spaces. Runes are
// preserved otherwise, including wide characters.
func String(s string) string {
	if !needsWork(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == 0x1b:
			i = skipEscape(s, i)
		case c < 0x20 && c != '\n' && c != '\t':
			b.WriteByte(' ')
			i++
		case c == 0x7f:
			b.WriteByte(' ')
			i++
		case c >= 0x80 && c < 0xa0: // C1 controls
			b.WriteByte(' ')
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size <= 1 {
				b.WriteByte(' ')
				i++
				continue
			}
			if isBidiControl(r) || isFormattingControl(r) {
				i += size
				continue
			}
			b.WriteString(s[i : i+size])
			i += size
		}
	}
	return b.String()
}

func needsWork(s string) bool {
	for _, r := range s {
		if r == 0x1b || (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r < 0xa0) {
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
		j++
		for j < len(s) {
			c := s[j]
			if c >= 0x40 && c <= 0x7e {
				return j + 1
			}
			j++
		}
		return j
	case ']': // OSC: terminated by BEL or ST (ESC \)
		j++
		for j < len(s) {
			if s[j] == 0x07 {
				return j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
			j++
		}
		return j
	case 'P', 'X', '^', '_': // DCS, SOS, PM, APC: terminated by ST
		j++
		for j < len(s) {
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
			j++
		}
		return j
	default:
		// two-byte C1 escape or other short sequences
		j++
		if j < len(s) && s[j] >= 0x20 && s[j] <= 0x7e {
			j++
		}
		return j
	}
}

// isBidiControl covers characters that can reorder rendered text invisibly.
func isBidiControl(r rune) bool {
	switch r {
	case 0x200e, 0x200f, 0x061c, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e,
		0x2066, 0x2067, 0x2068, 0x2069:
		return true
	}
	return false
}

func isFormattingControl(r rune) bool {
	return unicode.Is(unicode.Cf, r) && r != 0x200b
}

// LogLine collapses every control byte (including newlines) so one log
// record is one line, and strips escapes entirely.
func LogLine(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == 0x1b:
			i = skipEscape(s, i)
		case c < 0x20 || c == 0x7f:
			b.WriteByte(' ')
			i++
		case c >= 0x80 && c < 0xa0:
			b.WriteByte(' ')
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}
