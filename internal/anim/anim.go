// Package anim holds the UI's small time-based animations: a value easing
// toward a target on the caller's clock, so any widget rides the same ramp.
package anim

import (
	"time"

	"github.com/metafates/zemi/internal/hostapi"
)

// Fade eases a 0..1 value toward shown 1 or hidden 0 over Dur. Flipping
// mid-ramp starts the new ramp from the value's current place, so a pill
// that reappears while sinking rises from where it is. The zero value is
// settled hidden and never animates; NewFade gives the ramp its duration.
type Fade struct {
	Dur   time.Duration
	Delay time.Duration // how long an appear waits before its ramp starts
	shown bool
	at    time.Time
	from  float64
}

// NewFade is a Fade easing over dur.
func NewFade(dur time.Duration) *Fade { return &Fade{Dur: dur} }

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

// Faded is row with every segment but the ones spare keeps sunk toward the
// surface it sits on by t, keeping a deeper sink a row set itself.
func Faded(row []hostapi.Segment, t float64, spare func(hostapi.Segment) bool) []hostapi.Segment {
	out := make([]hostapi.Segment, len(row))
	for i, s := range row {
		if spare == nil || !spare(s) {
			s.Fade = max(s.Fade, t)
		}
		out[i] = s
	}
	return out
}

// FadedRows is rows with every row faded.
func FadedRows(rows [][]hostapi.Segment, t float64, spare func(hostapi.Segment) bool) [][]hostapi.Segment {
	out := make([][]hostapi.Segment, len(rows))
	for i, row := range rows {
		out[i] = Faded(row, t, spare)
	}
	return out
}

// Curtain is one thing that comes and goes on a fade: appearing rises out
// of the surface, leaving sinks back into it on the rows it kept drawing,
// and a return rises from where it sank. Draw hands it a frame's rows and
// hands back what the screen shows, so a widget never reads the ramp.
type Curtain struct {
	Dur    time.Duration
	JumpIn bool // an appear takes the rows whole: no rise

	fade    Fade
	rows    [][]hostapi.Segment // the rows a sink keeps drawing
	sinking bool
}

// NewCurtain is a Curtain easing over dur.
func NewCurtain(dur time.Duration) *Curtain { return &Curtain{Dur: dur, fade: Fade{Dur: dur}} }

// Draw settles a frame's rows through the curtain at now: the widget's rows
// while they ride full alpha, the kept rows while they sink, nil once the
// region is out. settled reports a sink that just rode out, once: what the
// sink covered behind it draws from the next frame on.
func (c *Curtain) Draw(rows [][]hostapi.Segment, now time.Time) (out [][]hostapi.Segment, settled bool) {
	if len(rows) > 0 {
		c.sinking = false
		if !c.fade.Shown() {
			if c.JumpIn {
				c.fade.Jump(true)
			} else {
				c.fade.Set(true, now)
			}
		}
		c.rows = rows
		if a := c.fade.Alpha(now); a < 1 {
			return FadedRows(rows, 1-a, nil), false
		}
		return rows, false
	}
	if c.fade.Shown() {
		c.fade.Set(false, now) // the sink starts where the rows stand
	}
	if a := c.fade.Alpha(now); a > 0 && len(c.rows) > 0 {
		c.sinking = true
		return FadedRows(c.rows, 1-a, nil), false
	}
	settled, c.sinking = c.sinking, false // the sink rode out: report it once
	return nil, settled
}

// Rising reports a ramp in flight at now, a sink included: it holds until
// the screen has seen the region out, so the tick keeps coming over a
// sink's end.
func (c *Curtain) Rising(now time.Time) bool {
	return c.sinking || c.fade.Rising(now)
}

// Sinking reports a sink still drawing the rows it kept.
func (c *Curtain) Sinking() bool { return c.sinking }

// Jump takes the curtain to its target with no ramp.
func (c *Curtain) Jump(shown bool) { c.fade.Jump(shown) }

// Settle jumps a ramp in flight to its target: a test's pinned clock would
// otherwise freeze it mid-ride. Reports whether it settled one.
func (c *Curtain) Settle(now time.Time) bool {
	if c.fade.Rising(now) {
		c.fade.Jump(c.fade.Shown())
		return true
	}
	return false
}

// Rise holds one appear per key: a widget, a rail call, a reply's readout.
// Two ways to drive it. Stamp for an element that rises once: the first
// Stamp stamps the ramp and later frames just read Alpha. Present for an
// element that comes and goes: report what the frame drew, then Prune, so a
// return rises again and a key gone loses its ramp at once. The zero value
// is empty; NewRise gives ramps their duration and their clock.
type Rise[K comparable] struct {
	Dur  time.Duration
	Now  func() time.Time // nil reads time.Now
	fade map[K]*Fade
	seen map[K]bool
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

// Stamp is the key's fade, stamped rising on its first sight. An element
// that rises once stamps on its first frame and only reads Alpha after.
func (r *Rise[K]) Stamp(key K) *Fade {
	if r.fade == nil {
		r.fade = map[K]*Fade{}
	}
	f, ok := r.fade[key]
	if !ok {
		f = NewFade(r.Dur)
		f.Set(true, r.clock())
		r.fade[key] = f
	}
	return f
}

// Present reports whether the frame drew the key: its first sight or a
// return after a Prune stamps a fresh ramp, a ramp in flight keeps riding,
// and a key already risen stays risen. Present with false flips a risen key
// toward hidden, sinking where it stands.
func (r *Rise[K]) Present(key K, shown bool) {
	if r.fade == nil {
		r.fade = map[K]*Fade{}
	}
	if r.seen == nil {
		r.seen = map[K]bool{}
	}
	now := r.clock()
	r.seen[key] = true
	f, ok := r.fade[key]
	switch {
	case !ok:
		f = NewFade(r.Dur)
		f.Set(shown, now)
		r.fade[key] = f
	case shown:
		if !f.Rising(now) && !f.Shown() {
			f.Set(true, now) // returned after a sink: rise from where it sank to
		}
	default:
		f.Set(false, now)
	}
}

// Prune ends a Present frame: keys the frame did not present lose their ramp
// at once. Only an element that vanishes without a sink prunes.
func (r *Rise[K]) Prune() {
	for k := range r.fade {
		if !r.seen[k] {
			delete(r.fade, k)
		}
	}
	clear(r.seen)
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
