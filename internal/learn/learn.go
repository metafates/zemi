// Package learn is the M2 learning core (DESIGN §10): memory intake with
// the §10.4 policy (length cap, kind enum, provenance, secret scrubbing,
// instruction-shaped flagging) and the §10.7 digest builder. Autonomy
// gating and the review queue live above it, in hostapi.
package learn

import "errors"

// MaxTextLen caps memory text at intake (DESIGN §10.4).
const MaxTextLen = 512

// Kinds is the fixed intake enum (DESIGN §10.4).
var Kinds = []string{"fact", "gotcha", "preference", "convention", "decision"}

// OriginUser and OriginLearned are the memory.origin values (DESIGN §7);
// the digest labels and the supersede guard read them.
const (
	OriginUser    = "user"
	OriginLearned = "learned"
)

// ErrRefused marks text the intake policy will not store.
var ErrRefused = errors.New("learn: intake refused")

// ErrBadKind marks a kind outside the fixed enum.
var ErrBadKind = errors.New("learn: kind outside the enum")

// ErrBadOrigin marks an origin that is not user or learned.
var ErrBadOrigin = errors.New("learn: origin must be user or learned")

// ErrNoProvenance marks missing provenance (DESIGN §10.4 requires it).
var ErrNoProvenance = errors.New("learn: provenance required")

func validKind(k string) bool {
	for _, v := range Kinds {
		if v == k {
			return true
		}
	}
	return false
}
