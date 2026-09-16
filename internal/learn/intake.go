// intake.go applies the §10.4 memory intake policy host-side, whatever the
// autonomy mode: sanitize and cap the text, scrub secrets, scrub tags,
// require kind/provenance/origin, and flag instruction-shaped text for the
// review queue.
package learn

import (
	"fmt"
	"slices"
	"strings"

	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
)

// MaxTags caps the tag list; MaxTagLen caps one tag.
const (
	MaxTags   = 8
	MaxTagLen = 32
)

// Result is a policy-clean memory ready to store, plus the flags the
// autonomy gate needs.
type Result struct {
	Memory            store.Memory
	Scrubbed          bool   // a secret span was redacted
	InstructionShaped bool   // routes to the review queue even under auto
	Supersedes        string // id of the memory this one replaces, linked when stored
}

// Intake validates and cleans one memory (DESIGN §10.4). Errors are
// ErrRefused (one-blob secrets), ErrBadKind, ErrBadOrigin or
// ErrNoProvenance; everything else is cleaned into the result.
func Intake(scope, origin, kind, text string, tags []string, provenance string) (*Result, error) {
	if !slices.Contains(Kinds, kind) {
		return nil, ErrBadKind
	}
	if origin != OriginUser && origin != OriginLearned {
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

	return &Result{
		Memory: store.Memory{
			Scope:               scope,
			Kind:                kind,
			Text:                clean,
			Tags:                cleanTags(tags),
			ProvenanceSessionID: provenance,
			Origin:              origin,
		},
		Scrubbed:          scrubbed,
		InstructionShaped: InstructionShaped(clean),
	}, nil
}

// cleanTags sanitizes, dedupes and caps tags; scrubbed like text (§10.4).
func cleanTags(tags []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tags {
		t, scrubbed := scrubCapped(t, MaxTagLen)
		// a tag that held a secret is junk as a tag; text keeps its markers
		if t == "" || seen[t] || scrubbed {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) == MaxTags {
			break
		}
	}
	return out
}

// scrubCapped sanitizes s to one line, scrubs it and cuts it to n runes.
// The scrub runs before the cut, which could leave a secret too short to
// recognize; the first cut at 2n only bounds the scrub's work.
func scrubCapped(s string, n int) (string, bool) {
	out, scrubbed := ScrubSecrets(Truncate(strings.Join(strings.Fields(sanitize.String(s)), " "), 2*n))
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
