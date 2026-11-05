package anim

import (
	"testing"
	"time"

	"github.com/metafates/zemi/internal/hostapi"
)

// TestFade: a fade rises from the value it had, a flip mid-ramp resumes
// from where the value is, and Jump settles with no ramp.
func TestFade(t *testing.T) {
	now := time.UnixMilli(0)
	f := NewFade(100 * time.Millisecond)
	if f.Alpha(now) != 0 || f.Rising(now) || f.Shown() {
		t.Fatal("a fresh fade draws hidden and settled")
	}
	f.Set(true, now)
	if got := f.Alpha(now.Add(50 * time.Millisecond)); got < 0.4 || got > 0.6 {
		t.Fatalf("mid-ramp alpha %v, want about half", got)
	}
	mid := now.Add(50 * time.Millisecond)
	f.Set(false, mid)
	if got := f.Alpha(mid); got < 0.4 || got > 0.6 {
		t.Fatalf("a flip did not start from the value it had: %v", got)
	}
	if got := f.Alpha(mid.Add(2 * f.Dur)); got != 0 || f.Rising(mid.Add(2*f.Dur)) {
		t.Fatalf("the reversed ramp did not settle hidden: %v", got)
	}
	f.Jump(true)
	if f.Alpha(mid) != 1 || f.Rising(mid) || !f.Shown() {
		t.Fatal("a jumped fade is settled at its target")
	}

	// a fade with no duration reads its target at once, never NaN
	z := &Fade{}
	if got := z.Alpha(now); got != 0 {
		t.Fatalf("a zero fade is not settled hidden: %v", got)
	}
	z.Set(true, now)
	if got := z.Alpha(now); got != 1 {
		t.Fatalf("a durationless fade did not read shown: %v", got)
	}

	// an Ease replaces the default easing
	e := NewFade(100 * time.Millisecond)
	e.Ease = func(p float64) float64 { return p / 2 }
	e.Set(true, now)
	if got := e.Alpha(now.Add(50 * time.Millisecond)); got != 0.25 {
		t.Fatalf("the ease was not honored: %v", got)
	}

	// a delayed appear holds its start value through the delay, then ramps
	d := NewFade(100 * time.Millisecond)
	d.Delay = 60 * time.Millisecond
	d.Set(true, now)
	if got := d.Alpha(now.Add(30 * time.Millisecond)); got != 0 || !d.Rising(now.Add(30*time.Millisecond)) {
		t.Fatalf("the delay did not hold the appear sunk: %v", got)
	}
	if got := d.Alpha(now.Add(110 * time.Millisecond)); got < 0.4 || got > 0.6 {
		t.Fatalf("mid-ramp alpha %v, want about half", got)
	}
	if d.Rising(now.Add(160 * time.Millisecond)) {
		t.Fatal("the delayed ramp did not settle")
	}
}

// TestCurtain: a curtain appears rising, leaves sinking on the rows it kept
// drawing, returns rising from where it sank, and Rising holds until the
// screen has seen the region out.
func TestCurtain(t *testing.T) {
	now := time.UnixMilli(0)
	row := func() [][]hostapi.Segment {
		return [][]hostapi.Segment{{{Text: "gone", HL: "Dim"}}}
	}
	c := NewCurtain(100 * time.Millisecond)
	if got, _ := c.Draw(row(), now); got == nil || got[0][0].Fade != 1 {
		t.Fatalf("a fresh curtain's first draw is not sunk: %v", got)
	}
	if !c.Rising(now) {
		t.Fatal("a fresh curtain is not rising")
	}
	if got, _ := c.Draw(row(), now.Add(2*c.Dur)); got == nil || got[0][0].Fade != 0 {
		t.Fatal("the risen curtain did not draw its rows clean")
	}

	// the leave sinks on the rows it kept, and Rising holds until a draw saw
	// the region out: a sink that expires unseen still holds the tick
	if got, _ := c.Draw(nil, now.Add(2*c.Dur)); got == nil || got[0][0].Fade > 0.5 {
		t.Fatal("a leaving curtain did not start its sink from the kept rows")
	}
	if got, _ := c.Draw(nil, now.Add(2*c.Dur).Add(c.Dur/2)); got == nil || got[0][0].Fade < 0.4 || got[0][0].Fade > 0.6 {
		t.Fatalf("the sink did not ride mid-ramp: %v", got)
	}
	if !c.Rising(now.Add(4 * c.Dur)) {
		t.Fatal("an unseen sink's end does not keep the curtain rising")
	}
	if got, settled := c.Draw(nil, now.Add(4*c.Dur)); got != nil || !settled {
		t.Fatalf("the sink did not ride out: %v settled %v", got, settled)
	}
	if _, settled := c.Draw(nil, now.Add(4*c.Dur)); settled {
		t.Fatal("the sink's end was reported twice")
	}

	// a return rises from where it sank
	if got, _ := c.Draw(row(), now.Add(4*c.Dur)); got == nil || got[0][0].Fade != 1 {
		t.Fatal("a returned curtain did not rise from sunk")
	}
	if got, _ := c.Draw(row(), now.Add(4*c.Dur).Add(c.Dur/2)); got == nil || got[0][0].Fade < 0.4 || got[0][0].Fade > 0.6 {
		t.Fatalf("the returned curtain did not ride its rise: %v", got)
	}

	// JumpIn takes an appear whole
	j := NewCurtain(100 * time.Millisecond)
	j.JumpIn = true
	if got, _ := j.Draw(row(), now); got == nil || got[0][0].Fade != 0 {
		t.Fatal("a jump-in curtain's first draw rose")
	}
}

// TestFaded: a t of 0 or less hands the row back, a t fades every segment
// but the ones spare keeps, and a row's own deeper sink stands.
func TestFaded(t *testing.T) {
	row := []hostapi.Segment{{Text: "a"}, {Text: "b", Fade: 0.9}, {Text: "c", HL: "Bar"}}
	if got := Faded(row, 0, nil); &got[0] != &row[0] {
		t.Fatal("a t of 0 did not hand the row back")
	}
	spares := func(s hostapi.Segment) bool { return s.HL == "Bar" }
	got := Faded(row, 0.4, spares)
	if got[0].Fade != 0.4 || got[1].Fade != 0.9 || got[2].Fade != 0 {
		t.Fatalf("the fade ignored the spare or a deeper sink: %v", got)
	}
	if row[0].Fade != 0 {
		t.Fatal("fading wrote into the row it was handed")
	}
}

// TestRise: a key's first Present stamps its ramp, a Present while it rides
// keeps it, a risen key stays risen, Prune drops the keys a frame did not
// present, a sunk key rises again, and Stamp rises an element that never
// leaves exactly once.
func TestRise(t *testing.T) {
	now := time.UnixMilli(0)
	clock := now
	r := NewRise[string](100*time.Millisecond, func() time.Time { return clock })
	if r.Alpha("k") != 0 || r.Rising() {
		t.Fatal("an empty rise reads hidden and settled")
	}
	r.Present("k", true)
	if got := r.Alpha("k"); got != 0 {
		t.Fatalf("a fresh key is not sunk: %v", got)
	}
	clock = now.Add(50 * time.Millisecond)
	if got := r.Alpha("k"); got < 0.4 || got > 0.6 {
		t.Fatalf("mid-ramp alpha %v, want about half", got)
	}
	r.Present("k", true)
	if got := r.Alpha("k"); got < 0.4 || got > 0.6 {
		t.Fatalf("a present did not leave the ramp in flight: %v", got)
	}
	clock = now.Add(200 * time.Millisecond)
	if r.Rising() || r.Alpha("k") != 1 {
		t.Fatal("the ramp did not settle shown")
	}
	r.Present("k", true)
	if got := r.Alpha("k"); got != 1 {
		t.Fatalf("a present of a risen key did not leave it risen: %v", got)
	}
	r.Present("k", true)
	r.Prune()
	r.Present("k", true)
	r.Present("gone", true)
	r.Prune()
	if r.Alpha("gone") != 0 {
		t.Fatal("prune did not drop the key the frame did not present")
	}
	if r.Alpha("k") != 1 {
		t.Fatal("prune dropped a key the frame presented")
	}
	r.Present("k", false)
	clock = now.Add(400 * time.Millisecond)
	if r.Alpha("k") != 0 || r.Rising() {
		t.Fatal("a present-false did not sink the key")
	}
	r.Present("k", true)
	if got := r.Alpha("k"); got != 0 {
		t.Fatalf("a return after a sink did not rise again: %v", got)
	}

	s := NewRise[string](100*time.Millisecond, func() time.Time { return clock })
	f := s.Stamp("j")
	if f.Alpha(clock) != 0 {
		t.Fatal("a fresh stamp is not sunk")
	}
	clock = clock.Add(200 * time.Millisecond)
	if f2 := s.Stamp("j"); f2 != f || f.Alpha(clock) != 1 {
		t.Fatal("a re-stamp did not return the settled fade")
	}
	s2 := NewRise[string](100*time.Millisecond, func() time.Time { return clock })
	s2.Stamp("j")
	if !s2.Settle() || s2.Settle() {
		t.Fatal("settle did not land the ramp at its target")
	}

	// prune drops a key that finished sinking, keeps one still sinking
	r.Present("sunk", true)
	clock = clock.Add(10 * r.Dur)
	r.Prune()
	r.Present("sunk", false)
	clock = clock.Add(10 * r.Dur)
	r.Present("marker", true)
	r.Prune()
	if _, ok := r.fade["sunk"]; ok {
		t.Fatal("prune kept a sunk key's fade")
	}
	r.Present("sinking", true)
	clock = clock.Add(10 * r.Dur)
	r.Present("sinking", false)
	clock = clock.Add(r.Dur / 2)
	r.Present("elsewhere", true)
	r.Prune()
	if got := r.Alpha("sinking"); got != 0.5 {
		t.Fatalf("prune dropped a key still sinking, or its ramp moved: %v", got)
	}
}
