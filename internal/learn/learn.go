// Package learn is the learning core (DESIGN §10): memory intake with
// the §10.4 policy (length caps, provenance, secret scrubbing,
// instruction-shaped flagging), the near-duplicate check and the §10.7
// digest. Autonomy gating and the review queue live above it, in hostapi.
package learn

import "errors"

// MaxTextLen caps memory text and MaxQuoteLen the user's words behind it,
// in runes (DESIGN §10.4).
const (
	MaxTextLen  = 300
	MaxQuoteLen = 160
)

// ScopeBudget caps the bytes one scope's lines take in the prompt: every
// live memory is there, so the write gate refuses to grow past it
// (DESIGN §10.7).
const ScopeBudget = 3072

// ErrRefused marks text the intake policy will not store.
var ErrRefused = errors.New("learn: intake refused")

// ErrBadOrigin marks an origin that is not user or learned.
var ErrBadOrigin = errors.New("learn: origin must be user or learned")

// ErrNoProvenance marks missing provenance (DESIGN §10.4 requires it).
var ErrNoProvenance = errors.New("learn: provenance required")
