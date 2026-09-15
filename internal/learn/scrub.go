// scrub.go redacts secret-shaped spans at intake (DESIGN §10.4): known key
// shapes, key=value assignments and spans above an entropy threshold are
// replaced with a "[redacted n chars]" marker. Text that is one secret
// blob after scrubbing is refused, not stored.
package learn

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

// RedactedMarker is the placeholder left in place of a redacted span.
const RedactedMarker = "[redacted %d chars]"

var keyShapes = []*regexp.Regexp{
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,}\b`),
	regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`),
	regexp.MustCompile(`\b[A-Za-z0-9+/_=-]{40,}\b`),
}

var keyAssignments = regexp.MustCompile(
	`(?i)\b(api[_-]?key|apikey|access[_-]?token|auth[_-]?token|token|secret|password|passwd|pwd|bearer|authorization)\b\s*[=:]\s*\S+`)

// entropyThreshold is the shannon-entropy floor (bits/char) above which a
// long token is presumed secret; entropyFloorLen the minimum token length
// the entropy pass considers.
const (
	entropyThreshold = 4.5
	entropyFloorLen  = 20
)

// ScrubSecrets replaces secret-shaped spans with redaction markers and
// reports whether anything was redacted. Redaction is per span, so
// "[redacted 24 chars]" markers can coexist with surviving prose.
func ScrubSecrets(text string) (string, bool) {
	out := text
	scrubbed := false
	for _, re := range keyShapes {
		out, scrubbed = scrubMatches(out, re, scrubbed)
	}
	out, scrubbed = scrubMatches(out, keyAssignments, scrubbed)
	for _, tok := range strings.Fields(out) {
		if len(tok) < entropyFloorLen || shannon(tok) < entropyThreshold {
			continue
		}
		out = strings.ReplaceAll(out, tok, redactMarker(len(tok)))
		scrubbed = true
	}
	return out, scrubbed
}

// oneBlob strips every redaction marker; what remains must be whitespace.
var oneBlob = regexp.MustCompile(`\[redacted \d+ chars\]`)

// IsOneBlob reports whether scrubbed text carries no surviving content,
// the §10.4 "one blob" refusal shape.
func IsOneBlob(scrubbed string) bool {
	return strings.TrimSpace(oneBlob.ReplaceAllString(scrubbed, "")) == ""
}

func scrubMatches(s string, re *regexp.Regexp, scrubbed bool) (string, bool) {
	ms := re.FindAllString(s, -1)
	if len(ms) == 0 {
		return s, scrubbed
	}
	for _, m := range ms {
		s = strings.ReplaceAll(s, m, redactMarker(utf8.RuneCountInString(m)))
	}
	return s, true
}

func redactMarker(n int) string {
	return fmt.Sprintf(RedactedMarker, n)
}

// shannon computes bits of entropy per byte of s.
func shannon(s string) float64 {
	counts := make(map[byte]int, 64)
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	h := 0.0
	n := float64(len(s))
	for _, c := range counts {
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
