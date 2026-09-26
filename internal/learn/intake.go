// intake.go applies the §10.4 memory intake policy host-side, whatever the
// autonomy mode: sanitize and cap the text and the quote, scrub secrets,
// require provenance and origin, and flag instruction-shaped text for the
// review queue.
package learn

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
)

// Result is a policy-clean memory ready to store, plus the flags the
// autonomy gate needs.
type Result struct {
	Memory            store.Memory
	Scrubbed          bool   // a secret span of the quote was redacted
	InstructionShaped bool   // routes to the review queue even under auto
	Supersedes        string // id of the memory this one replaces, linked when stored
}

// Intake validates and cleans one memory (DESIGN §10.4). Errors are
// ErrRefused (empty text, a secret in the text), ErrBadOrigin or
// ErrNoProvenance; everything else is cleaned into the result. A text
// with a secret is refused rather than stored redacted: the digest never
// loads a redacted row, so storing one only says "saved" for a memory
// nobody will read.
func Intake(scope store.Scope, origin store.Origin, text, quote, provenance string) (*Result, error) {
	if !slices.Contains(store.Origins, origin) {
		return nil, ErrBadOrigin
	}
	if strings.TrimSpace(provenance) == "" {
		return nil, ErrNoProvenance
	}

	clean, scrubbed := scrubCapped(text, MaxTextLen)
	if clean == "" {
		return nil, fmt.Errorf("%w: empty text", ErrRefused)
	}
	if IsOneBlob(clean) {
		return nil, fmt.Errorf("%w: text is one secret blob", ErrRefused)
	}
	if scrubbed {
		return nil, fmt.Errorf("%w: a secret was redacted and a redacted memory never loads; name the env var that holds it instead", ErrRefused)
	}
	q, qs := scrubCapped(quote, MaxQuoteLen)

	return &Result{
		Memory: store.Memory{
			Scope:               scope,
			Text:                clean,
			Quote:               q,
			ProvenanceSessionID: provenance,
			Origin:              origin,
		},
		Scrubbed:          qs,
		InstructionShaped: InstructionShaped(clean),
	}, nil
}

// scrubCapped sanitizes s to one line, scrubs it and cuts it to n runes.
// The scrub runs before the cut, which could leave a secret too short to
// recognize; the first cut at 2n only bounds the scrub's work.
func scrubCapped(s string, n int) (string, bool) {
	out, scrubbed := ScrubSecrets(Truncate(sanitize.Line(s), 2*n))
	return strings.TrimSpace(Truncate(out, n)), scrubbed
}

// Truncate cuts s to at most n runes.
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// contentToken is a word of three or more letters or any run with a digit,
// so a different short name (vet, api) or number keeps two facts apart.
var contentToken = regexp.MustCompile(`[a-z0-9]*[0-9][a-z0-9]*|[a-z]{3,}`)

// Similar reports whether two memory texts say the same thing: the same
// text but for case, or 75% of their content tokens in common, a word
// compared by its first five letters so "regenerates" matches "regenerated".
// Empty token sets never match.
// NOTE: token overlap only; the reflector's full index is the semantic check.
func Similar(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	ta, tb := tokens(a), tokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return false
	}
	both := 0
	for t := range ta {
		if tb[t] {
			both++
		}
	}
	return 4*both >= 3*(len(ta)+len(tb)-both)
}

func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range contentToken.FindAllString(strings.ToLower(s), -1) {
		if len(t) > 5 && !strings.ContainsAny(t, "0123456789") {
			t = t[:5]
		}
		out[t] = true
	}
	return out
}
