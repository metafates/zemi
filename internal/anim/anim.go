// Package anim holds the UI's small time-based animations: a value easing
// toward a target on the caller's clock, so any widget rides the same ramp.
//
// Turning a static element into an animated one takes one of three patterns.
//
// An element that comes and goes with its widget (a pill, a splash, a chat
// region) rides a Curtain: the widget draws its rows as always and hands
// them through Draw, which rises them out of the surface on appear and
// sinks them on the rows it kept on leave.
//
//	c := anim.NewCurtain(160 * time.Millisecond)
//	out, _ := c.Draw(rows, now) // out is what the screen shows
//
// Words that change in place (a pill's counts) dip through the same
// curtain: Dip sinks the drawn rows and the next Draw's rows rise once it
// lands, so a value never swaps mid-fade. The dip dissolves only the cells
// the words changed: text the two draws share stands, the span between the
// common prefix and suffix fades — a count that moves one digit animates
// that digit alone.
//
// A curtain whose first paint must wait out a delay (the chat view's boot,
// the scrollbar's) stamps it with Boot instead of drawing into a JumpIn.
// An element that appears and may go away within one widget's rows (a rail
// call, a readout) rides a Rise keyed by what names it: report the keys the
// frame drew with Present, fade the rows by Alpha while it is under 1, then
// Prune so a key gone loses its ramp.
//
//	f := rise.Present(key, true)
//	if a := rise.Alpha(key); a < 1 {
//		row = anim.Faded(row, 1-a, nil)
//	}
//
// An element that rises exactly once stamps the same way and never prunes;
// Rise drops a key that finished sinking on its own.
//
// All types are for one goroutine: the render's. The zero value of every
// type is settled hidden and safe; NewFade, NewCurtain and NewRise give a
// ramp its duration.
package anim

import (
	"slices"
	"time"

	"github.com/metafates/zemi/internal/hostapi"
)

// Fade eases a 0..1 value toward shown 1 or hidden 0 over Dur. Flipping
// mid-ramp starts the new ramp from the value's current place, so a pill
// that reappears while sinking rises from where it is. The zero value is
// settled hidden and never animates; NewFade gives the ramp its duration.
// A Dur of 0 or less jumps: Alpha reads the target at once.
type Fade struct {
	Dur   time.Duration
	Delay time.Duration // how long an appear waits before its ramp starts
	Ease  func(float64) float64
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
	dur := max(f.Dur, 0)
	if dur <= 0 {
		return target
	}
	p := float64(now.Sub(f.at)-delay) / float64(dur)
	if p >= 1 {
		return target
	}
	if p <= 0 {
		return f.from
	}
	ease := f.Ease
	if ease == nil {
		ease = Smoothstep
	}
	return min(max(f.from+(target-f.from)*ease(p), 0), 1)
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
// surface it sits on by t, keeping a deeper sink a row set itself. A t of 0
// or less hands the row back untouched.
func Faded(row []hostapi.Segment, t float64, spare func(hostapi.Segment) bool) []hostapi.Segment {
	if t <= 0 {
		return row
	}
	out := make([]hostapi.Segment, len(row))
	for i, s := range row {
		if spare == nil || !spare(s) {
			s.Fade = max(s.Fade, t)
		}
		out[i] = s
	}
	return out
}

// Tinted is row with every segment standing by t in the colors its From group
// holds, 0 being the segment's own. A t of 0 or less hands the row back
// untouched.
func Tinted(row []hostapi.Segment, t float64) []hostapi.Segment {
	if t <= 0 {
		return row
	}
	out := make([]hostapi.Segment, len(row))
	for i, s := range row {
		s.Rise = t
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

// dissolveRows builds one frame of a dip between the rows it sinks from and
// the rows it rises to: a row the two share stands whole, and a row they do
// not keeps the text before and after the changed span at full alpha while
// the span itself fades — the old words' span while they sink, the new
// words' while they rise. A row with no mate sinks whole.
func dissolveRows(prev, next [][]hostapi.Segment, sunk float64, rise bool) [][]hostapi.Segment {
	shown, mate := prev, next
	if rise {
		shown, mate = next, prev
	}
	out := make([][]hostapi.Segment, len(shown))
	for i, row := range shown {
		var other []hostapi.Segment
		if i < len(mate) {
			other = mate[i]
		}
		out[i] = dissolveRow(row, other, sunk)
	}
	return out
}

// dissolveRow is row sunk toward its surface on the span where its text
// leaves other's, the shared text standing. The span is the text between the
// common prefix and suffix, so a count that changes one digit dissolves that
// digit alone.
func dissolveRow(row, other []hostapi.Segment, sunk float64) []hostapi.Segment {
	if sunk <= 0 {
		return row
	}
	if other == nil {
		return Faded(row, sunk, nil)
	}
	a, b := []rune(hostapi.SegsText(row)), []rune(hostapi.SegsText(other))
	if slices.Equal(a, b) {
		return row
	}
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	out := make([]hostapi.Segment, 0, len(row)+2)
	off := 0
	for _, seg := range row {
		rs := []rune(seg.Text)
		start, end := off, off+len(rs)
		off = end
		switch {
		case len(rs) == 0:
			out = append(out, seg)
		case end <= p || start >= len(a)-s:
			out = append(out, seg) // the shared text stands
		case start >= p && end <= len(a)-s:
			seg.Fade = max(seg.Fade, sunk)
			out = append(out, seg)
		default: // a segment the span cuts: the pieces outside stand, the piece inside fades
			head, mid, tail := splitSeg(seg, rs, p-start, len(a)-s-start)
			if head.Text != "" {
				out = append(out, head)
			}
			if mid.Text != "" {
				mid.Fade = max(mid.Fade, sunk)
				out = append(out, mid)
			}
			if tail.Text != "" {
				out = append(out, tail)
			}
		}
	}
	return out
}

// splitSeg cuts seg's runes at from and to (offsets into rs, clamped) into
// the pieces before, inside and past that span.
func splitSeg(seg hostapi.Segment, rs []rune, from, to int) (head, mid, tail hostapi.Segment) {
	if from < 0 {
		from = 0
	}
	if to > len(rs) {
		to = len(rs)
	}
	if from > 0 {
		head = seg
		head.Text = string(rs[:from])
	}
	if from < to {
		mid = seg
		mid.Text = string(rs[from:to])
	}
	if to < len(rs) {
		tail = seg
		tail.Text = string(rs[to:])
	}
	return head, mid, tail
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
	from    [][]hostapi.Segment // the rows a dip in flight dissolves from
	sinking bool
	dip     bool // a change's sink in flight: the kept rows sink, the next draw's rise
	drawn   bool // the curtain ever drew rows: it has owned its seat
}

// NewCurtain is a Curtain easing over dur.
func NewCurtain(dur time.Duration) *Curtain { return &Curtain{Dur: dur, fade: Fade{Dur: dur}} }

// Dip sinks the drawn rows over dur and the next Draw's rows rise once it
// lands, still over dur: an update that changes what a widget says dips it
// instead of swapping its bytes. A dur of 0 or less reads Dur. One on a
// hidden or sinking curtain does nothing; one mid-dip re-arms the sink from
// where it stands.
func (c *Curtain) Dip(dur time.Duration, now time.Time) {
	if !c.fade.Shown() || c.sinking {
		return
	}
	if dur <= 0 {
		dur = c.Dur
	}
	c.fade.Delay = 0 // a change's rise is not a boot's first paint: it starts at once
	c.from, c.dip, c.fade.Dur = c.rows, true, dur
	c.fade.Set(false, now) // the sink starts where the rows stand
}

// Owned reports a curtain that ever drew rows: the seat it took.
func (c *Curtain) Owned() bool { return c.drawn }

// Dipping reports a dip in flight: the sink it armed, or the rise that
// follows it.
func (c *Curtain) Dipping() bool { return c.dip }

// Boot stamps the curtain's first appear to wait delay and rise, where its
// JumpIn would take the rows whole: the chat view rises on boot like the
// scrollbar's first paint. A seat taken back from a sibling draws whole.
func (c *Curtain) Boot(delay time.Duration, now time.Time) {
	if c.drawn {
		return
	}
	c.fade.Delay = delay
	c.fade.Set(true, now)
}

// Draw settles a frame's rows through the curtain at now: the widget's rows
// while they ride full alpha, the kept rows while they sink, nil once the
// region is out. settled reports a sink that just rode out, once: what the
// sink covered behind it draws from the next frame on.
func (c *Curtain) Draw(rows [][]hostapi.Segment, now time.Time) (out [][]hostapi.Segment, settled bool) {
	if len(rows) > 0 {
		if c.dip {
			if a := c.fade.Alpha(now); a > 0 && !c.fade.Shown() && len(c.rows) > 0 {
				c.sinking = true
				return dissolveRows(c.rows, rows, 1-a, false), false // the changed cells of the old words, sinking
			}
			// the old words landed: the new ones rise against the rows they
			// replace, and a change mid-rise sinks them from where they stand
			c.sinking, c.drawn, c.rows = false, true, rows
			if !c.fade.Shown() {
				c.fade.Set(true, now)
			}
			if a := c.fade.Alpha(now); a < 1 {
				return dissolveRows(c.from, rows, 1-a, true), false
			}
			c.from = nil
			c.dip, c.fade.Dur = false, c.Dur
			return rows, false
		}
		c.sinking, c.drawn = false, true
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
	settled, c.sinking, c.rows = c.sinking, false, nil // the sink rode out: report it once, lose the kept rows
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

// Shown reports the curtain's target.
func (c *Curtain) Shown() bool { return c.fade.Shown() }

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
// return rises again and a key gone loses its ramp at once. An element that
// grows while it rises — a rail call gaining output rows — stamps by Spread:
// rows the earlier frames drew ride the ramp in flight, growth past it starts
// the key's next ramp. The zero value is empty; NewRise gives ramps their
// duration and their clock.
type Rise[K comparable] struct {
	Dur   time.Duration
	Now   func() time.Time // nil reads time.Now
	ramps map[K][]riseRamp
	seen  map[K]bool
}

// riseRamp is one of a key's ramps and how many of its rows it carries: the
// rows a frame drew for the key share the ramp their ordinals fall under.
type riseRamp struct {
	fade *Fade
	rows int
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

// stamp is the key's ramps with one rising stamped at now, if it had none.
func (r *Rise[K]) stamp(key K, now time.Time) []riseRamp {
	if r.ramps == nil {
		r.ramps = map[K][]riseRamp{}
	}
	if _, ok := r.ramps[key]; !ok {
		f := NewFade(r.Dur)
		f.Set(true, now)
		r.ramps[key] = []riseRamp{{fade: f, rows: 1}}
	}
	return r.ramps[key]
}

// Stamp is the key's fade, stamped rising on its first sight. An element
// that rises once stamps on its first frame and only reads Alpha after.
func (r *Rise[K]) Stamp(key K) *Fade {
	rs := r.stamp(key, r.clock())
	return rs[len(rs)-1].fade
}

// Spread is the key's alpha for its n-th row at now: rows the earlier frames
// drew ride the ramp that carried them, and a row past every ramp joins the
// newest one while it still rises or starts the key's next ramp, sunk.
func (r *Rise[K]) Spread(key K, n int) float64 {
	now := r.clock()
	rs := r.stamp(key, now)
	for _, ramp := range rs {
		if n < ramp.rows {
			return ramp.fade.Alpha(now)
		}
		n -= ramp.rows
	}
	last := &rs[len(rs)-1]
	if a := last.fade.Alpha(now); a < 1 {
		last.rows++
		return a
	}
	f := NewFade(r.Dur)
	f.Set(true, now)
	r.ramps[key] = append(rs, riseRamp{fade: f, rows: 1})
	return 0
}

// Present reports whether the frame drew the key: its first sight or a
// return after a Prune stamps a fresh ramp, a ramp in flight keeps riding,
// and a key already risen stays risen. Present with false flips a risen key
// toward hidden, sinking where it stands.
func (r *Rise[K]) Present(key K, shown bool) {
	if r.ramps == nil {
		r.ramps = map[K][]riseRamp{}
	}
	if r.seen == nil {
		r.seen = map[K]bool{}
	}
	now := r.clock()
	if shown {
		r.seen[key] = true
	}
	rs, ok := r.ramps[key]
	switch {
	case !ok:
		f := NewFade(r.Dur)
		f.Set(shown, now)
		r.ramps[key] = []riseRamp{{fade: f, rows: 1}}
	case shown:
		f := rs[len(rs)-1].fade
		if !f.Shown() {
			f.Set(true, now) // returned mid-sink: rise from where it sank to
		}
	default:
		rs[len(rs)-1].fade.Set(false, now)
	}
}

// Prune ends a Present frame: keys the frame did not present lose their ramps
// at once, and a key that finished sinking loses its ramps too, so a missed
// Prune cannot keep a ledger of dead keys. Only an element that vanishes
// without a sink prunes.
func (r *Rise[K]) Prune() {
	now := r.clock()
	for k, rs := range r.ramps {
		f := rs[len(rs)-1].fade
		if !r.seen[k] && (f.Shown() || f.Alpha(now) <= 0) {
			delete(r.ramps, k)
		}
	}
	clear(r.seen)
}

// Alpha is the key's value at now, 0 for a key that never appeared.
func (r *Rise[K]) Alpha(key K) float64 {
	rs := r.ramps[key]
	if len(rs) > 0 {
		return rs[len(rs)-1].fade.Alpha(r.clock())
	}
	return 0
}

// Retain drops every key keep refuses: an element the widget no longer draws
// has settled risen or left the screen, and only the keys still on it keep
// their ramps.
func (r *Rise[K]) Retain(keep func(K) bool) {
	for k := range r.ramps {
		if !keep(k) {
			delete(r.ramps, k)
		}
	}
}

// Rising reports a ramp in flight for any key.
func (r *Rise[K]) Rising() bool {
	if len(r.ramps) == 0 {
		return false
	}
	now := r.clock()
	for _, rs := range r.ramps {
		for _, ramp := range rs {
			if ramp.fade.Rising(now) {
				return true
			}
		}
	}
	return false
}

// Settle settles every ramp in flight at its target, reporting whether it
// settled one: a test's pinned clock would otherwise freeze a ramp mid-rise.
func (r *Rise[K]) Settle() bool {
	if len(r.ramps) == 0 {
		return false
	}
	now, done := r.clock(), false
	for _, rs := range r.ramps {
		for _, ramp := range rs {
			if ramp.fade.Rising(now) {
				ramp.fade.Jump(ramp.fade.Shown())
				done = true
			}
		}
	}
	return done
}

// Clear empties the ledger.
func (r *Rise[K]) Clear() { clear(r.ramps) }
