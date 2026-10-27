// clip.go is the shared text cutting: model-facing truncation, screen
// clipping and the one-line safe form.

package hostapi

import (
	"strings"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/sanitize"
)

// cutRunes cuts s to n runes, ending a cut one in suffix; trim drops the
// spaces before it, so the screens' ellipsis hugs the text.
func cutRunes(s string, n int, suffix string, trim bool) string {
	if c := sanitize.Truncate(s, n); c != s {
		if trim {
			c = strings.TrimRight(c, " ")
		}
		return c + suffix
	}
	return s
}

// truncRunes cuts text for the model or the log to n runes.
func truncRunes(s string, n int) string { return cutRunes(s, n, "...", false) }

// clipRunes cuts text the user reads to n runes, ending in the one-cell
// ellipsis every screen cuts with.
func clipRunes(s string, n int) string { return cutRunes(s, n, "…", true) }

// clipLine is s safe on one chat row: sanitized, its runs of space made one,
// cut as clipRunes cuts.
func clipLine(s string, n int) string {
	return clipRunes(sanitize.Line(s), n)
}

// concatText joins the text parts of a fantasy content slice.
func concatText[T any](parts []T) string {
	var b strings.Builder
	// Stored messages hold TextPart; model responses hold TextContent.
	for _, p := range parts {
		switch t := any(p).(type) {
		case fantasy.TextPart:
			b.WriteString(t.Text)
		case fantasy.TextContent:
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// redactedLines numbers the lines of clean that hold a redaction marker, for
// a recipe-cleaning note.
func redactedLines(clean string) []int {
	var lines []int
	for i, l := range strings.Split(clean, "\n") {
		if strings.Contains(l, "[redacted ") {
			lines = append(lines, i+1)
		}
	}
	return lines
}
