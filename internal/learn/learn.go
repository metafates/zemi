// Package learn is the learning core (DESIGN §10): memory intake with
// the §10.4 policy (length caps, provenance, secret scrubbing) and the
// near-duplicate check. Autonomy
// gating, the review queue and the injected render live above it, in
// hostapi and brain.
package learn

import "errors"

// MaxTextLen caps memory text and MaxQuoteLen the user's words behind it,
// in runes (DESIGN §10.4).
const (
	MaxTextLen  = 300
	MaxQuoteLen = 160
)

// ErrRefused marks text the intake policy will not store.
var ErrRefused = errors.New("learn: intake refused")

// ErrBadOrigin marks an origin that is not user or learned.
var ErrBadOrigin = errors.New("learn: origin must be user or learned")

// ErrNoProvenance marks missing provenance (DESIGN §10.4 requires it).
var ErrNoProvenance = errors.New("learn: provenance required")
