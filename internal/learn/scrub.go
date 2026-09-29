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

var keyShapes = []*regexp.Regexp{
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,}\b`),
	regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`),
	// no '/': long paths match otherwise; base64 with slashes falls to the
	// entropy pass. No '=': a long NAME=value is no key
	regexp.MustCompile(`\b[A-Za-z0-9+_-]{40,}\b`),
}

// urlPassword matches the userinfo of a URL (postgres://user:pass@host);
// group 1 is the password.
var urlPassword = regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s/:@]*:([^\s/]+)@`)

// keyAssignments matches a key name set to a value, group 2 the value; an
// auth scheme word before the value is skipped, so the credential is group 2.
var keyAssignments = regexp.MustCompile(
	`(?i)\b(api[_-]?key|apikey|access[_-]?token|auth[_-]?token|token|secret|password|passwd|pwd|bearer|authorization)\b\s*[=:]\s*(?:(?:bearer|basic|token)\s+)?(\S+)`,
)

// reference reports whether an assigned value names where the secret comes
// from ($VAR, ${VAR}, %VAR%, <token>, {{token}}) rather than holding it.
func reference(v string) bool {
	v = strings.TrimLeft(v, `"'`)
	return strings.HasPrefix(v, "{{") || v != "" && strings.ContainsRune("$%<", rune(v[0]))
}

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
	out := ScrubShapes(text)
	var toks []string
	for tok := range strings.FieldsSeq(out) {
		if len(tok) >= entropyFloorLen && shannon(tok) >= entropyThreshold {
			toks = append(toks, tok)
		}
	}
	out = redactAll(out, toks)
	return out, out != text
}

// ScrubShapes is ScrubSecrets without the entropy pass, for a recipe: there
// a long path or NAME=https://... is a command, and cutting it breaks the step.
// NOTE: a bare random token under 40 characters with no key name survives;
// the user reads every recipe write in its ask.
func ScrubShapes(text string) string {
	out := urlPassword.ReplaceAllStringFunc(text, func(m string) string {
		i := urlPassword.FindStringSubmatchIndex(m)
		return m[:i[2]] + redacted(m[i[2]:i[3]]) + m[i[3]:]
	})
	for _, re := range keyShapes {
		out = redactAll(out, re.FindAllString(out, -1))
	}
	var spans []string
	for _, m := range keyAssignments.FindAllStringSubmatch(out, -1) {
		if !reference(m[2]) {
			spans = append(spans, m[0])
		}
	}
	return redactAll(out, spans)
}

// oneBlob strips every redaction marker; what remains must be whitespace.
var oneBlob = regexp.MustCompile(`\[redacted \d+ chars\]`)

// IsOneBlob reports whether scrubbed text holds nothing but redaction
// markers: a recipe that was only secret material.
func IsOneBlob(scrubbed string) bool {
	return strings.TrimSpace(oneBlob.ReplaceAllString(scrubbed, "")) == ""
}

// redactAll replaces every occurrence of each span in one pass over s, so a
// secret found once is also redacted where it recurs glued to other text.
func redactAll(s string, spans []string) string {
	if len(spans) == 0 {
		return s
	}
	pairs := make([]string, 0, 2*len(spans))
	for _, m := range spans {
		pairs = append(pairs, m, redacted(m))
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

// redacted is the marker left in place of span.
func redacted(span string) string {
	return fmt.Sprintf("[redacted %d chars]", utf8.RuneCountInString(span))
}

// shannon computes bits of entropy per byte of s.
func shannon(s string) float64 {
	counts := make(map[byte]int, 64)
	for i := range len(s) {
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
