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
