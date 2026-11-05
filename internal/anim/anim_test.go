package anim

import (
	"testing"
	"time"
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
}
