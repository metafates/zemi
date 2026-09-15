// intake.go applies the §10.4 memory intake policy host-side, whatever the
// autonomy mode: sanitize and cap the text, scrub secrets, scrub tags,
// require kind/provenance/origin, and flag instruction-shaped text for the
// review queue.
package learn

import (
	"fmt"
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
	Scrubbed          bool // a secret span was redacted
	InstructionShaped bool // routes to the review queue even under auto
}

// Intake validates and cleans one memory (DESIGN §10.4). Errors are
// ErrRefused (one-blob secrets), ErrBadKind, ErrBadOrigin or
// ErrNoProvenance; everything else is cleaned into the result.
func Intake(scope, origin, kind, text string, tags []string, provenance string) (*Result, error) {
	if !validKind(kind) {
		return nil, ErrBadKind
	}
	if origin != OriginUser && origin != OriginLearned {
		return nil, ErrBadOrigin
	}
	if strings.TrimSpace(provenance) == "" {
		return nil, ErrNoProvenance
	}

	clean := collapse(sanitize.String(text))
	if runes := utf8Len(clean); runes > MaxTextLen {
		clean = truncate(clean, MaxTextLen)
	}
	clean, scrubbed := ScrubSecrets(clean)
	clean = strings.TrimSpace(clean)
	if clean == "" {
		return nil, fmt.Errorf("%w: empty text", ErrRefused)
	}
	if IsOneBlob(clean) {
		return nil, fmt.Errorf("%w: text is one secret blob", ErrRefused)
	}

	m := store.Memory{
		Scope:               scope,
		Kind:                kind,
		Text:                clean,
		Tags:                cleanTags(tags),
		ProvenanceSessionID: provenance,
		Origin:              origin,
	}
	return &Result{
		Memory:            m,
		Scrubbed:          scrubbed,
		InstructionShaped: InstructionShaped(clean),
	}, nil
}

// cleanTags sanitizes, dedupes and caps tags; scrubbed like text (§10.4).
func cleanTags(tags []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tags {
		t = collapse(sanitize.String(t))
		if runes := utf8Len(t); runes > MaxTagLen {
			t = truncate(t, MaxTagLen)
		}
		t, _ = ScrubSecrets(t)
		t = strings.TrimSpace(t)
		// A tag that kept a redaction marker is junk as a tag; text keeps
		// its markers, tags do not.
		if t == "" || seen[t] || IsOneBlob(t) || strings.Contains(t, "[redacted") {
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

// collapse turns every control/whitespace run into one space; memory text
// is single-line by construction.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func utf8Len(s string) int {
	return len([]rune(s))
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
