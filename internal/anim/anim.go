// Package anim holds the UI's small time-based animations: a value easing
// toward a target on the caller's clock, so any widget rides the same ramp.
package anim

import "time"

// Fade eases a 0..1 value toward shown 1 or hidden 0 over Dur. Flipping
// mid-ramp starts the new ramp from the value's current place, so a pill
// that reappears while sinking rises from where it is. The zero value is
// settled hidden and never animates; New gives the ramp its duration.
type Fade struct {
	Dur   time.Duration
	Delay time.Duration // how long an appear waits before its ramp starts
	shown bool
	at    time.Time
	from  float64
}

// New is a Fade easing over dur.
func New(dur time.Duration) *Fade { return &Fade{Dur: dur} }

// Set flips the fade's target at now, starting the ramp from the value's
// current place.
func (f *Fade) Set(shown bool, now time.Time) {
	if f.shown == shown {
		return
	}
	f.from = f.Alpha(now)
	f.shown, f.at = shown, now
}

// Jump moves to the target with no ramp.
func (f *Fade) Jump(shown bool) {
	f.shown, f.at, f.from = shown, time.Time{}, 0
}

// Alpha is the fade's value at now.
func (f *Fade) Alpha(now time.Time) float64 {
	target := 0.0
	var delay time.Duration
	if f.shown {
		target, delay = 1, f.Delay
	}
	p := float64(now.Sub(f.at)-delay) / float64(f.Dur)
	if p >= 1 {
		return target
	}
	if p <= 0 {
		return f.from
	}
	return f.from + (target-f.from)*Smoothstep(p)
}

// Rising reports a ramp in flight at now, an appear's delay included.
func (f *Fade) Rising(now time.Time) bool {
	d := now.Sub(f.at)
	return d >= 0 && d < f.Dur+f.Delay
}

// Shown is the fade's target.
func (f *Fade) Shown() bool { return f.shown }

// Smoothstep is the fades' easing: slow at both ends, quick in the middle.
func Smoothstep(p float64) float64 { return p * p * (3 - 2*p) }

// Rise holds one appear per key: a widget, a rail call, a reply's readout.
// The first frame that asks after a key stamps its ramp, so a row drawn for
// the first time rises out of the surface; a key gone loses its ramp. The
// zero value is empty; NewRise gives ramps their duration and their clock.
type Rise[K comparable] struct {
	Dur  time.Duration
	Now  func() time.Time // nil reads time.Now
	fade map[K]*Fade
}

// NewRise is a Rise easing each key's appear over dur, read on now.
func NewRise[K comparable](dur time.Duration, now func() time.Time) *Rise[K] {
	return &Rise[K]{Dur: dur, Now: now}
}

func (r *Rise[K]) clock() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Get is the key's fade, nil before its first sight.
func (r *Rise[K]) Get(key K) *Fade { return r.fade[key] }

// At is the key's fade, stamped rising on its first sight.
func (r *Rise[K]) At(key K) *Fade {
	if r.fade == nil {
		r.fade = map[K]*Fade{}
	}
	f, ok := r.fade[key]
	if !ok {
		f = New(r.Dur)
		f.Set(true, r.clock())
		r.fade[key] = f
	}
	return f
}

// Poke starts the key's ramp unless one is in flight: an appear stamps, a
// row riding its ramp keeps it.
func (r *Rise[K]) Poke(key K) {
	now := r.clock()
	if f := r.fade[key]; f != nil && f.Rising(now) {
		return
	}
	if r.fade == nil {
		r.fade = map[K]*Fade{}
	}
	f := New(r.Dur)
	f.Set(true, now)
	r.fade[key] = f
}

// Alpha is the key's value at now, 0 for a key that never appeared.
func (r *Rise[K]) Alpha(key K) float64 {
	if f := r.fade[key]; f != nil {
		return f.Alpha(r.clock())
	}
	return 0
}

// Rising reports a ramp in flight for any key.
func (r *Rise[K]) Rising() bool {
	if len(r.fade) == 0 {
		return false
	}
	now := r.clock()
	for _, f := range r.fade {
		if f.Rising(now) {
			return true
		}
	}
	return false
}

// Keep drops every key the predicate turns away.
func (r *Rise[K]) Keep(keep func(K) bool) {
	for k := range r.fade {
		if !keep(k) {
			delete(r.fade, k)
		}
	}
}

// Settle settles every ramp in flight at its target, reporting whether it
// settled one: a test's pinned clock would otherwise freeze a ramp mid-rise.
func (r *Rise[K]) Settle() bool {
	if len(r.fade) == 0 {
		return false
	}
	now, done := r.clock(), false
	for _, f := range r.fade {
		if f.Rising(now) {
			f.Jump(f.Shown())
			done = true
		}
	}
	return done
}

// Clear empties the ledger.
func (r *Rise[K]) Clear() { clear(r.fade) }
