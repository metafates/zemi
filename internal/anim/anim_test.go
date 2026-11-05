package anim

import (
	"testing"
	"time"
)

// TestFade: a fade rises from the value it had, a flip mid-ramp resumes
// from where the value is, and Jump settles with no ramp.
func TestFade(t *testing.T) {
	now := time.UnixMilli(0)
	f := New(100 * time.Millisecond)
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
	d := New(100 * time.Millisecond)
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

// TestRise: a key's first sight stamps its ramp, Poke keeps a ramp in flight,
// a key gone loses its entry, and Settle lands every ramp at its target.
func TestRise(t *testing.T) {
	now := time.UnixMilli(0)
	clock := now
	r := NewRise[string](100*time.Millisecond, func() time.Time { return clock })
	if r.Alpha("k") != 0 || r.Get("k") != nil || r.Rising() {
		t.Fatal("an empty rise reads hidden and settled")
	}
	r.Poke("k")
	if got := r.Alpha("k"); got != 0 {
		t.Fatalf("a fresh key is not sunk: %v", got)
	}
	clock = now.Add(50 * time.Millisecond)
	if got := r.Alpha("k"); got < 0.4 || got > 0.6 {
		t.Fatalf("mid-ramp alpha %v, want about half", got)
	}
	r.Poke("k")
	if got := r.Alpha("k"); got < 0.4 || got > 0.6 {
		t.Fatalf("a poke did not leave the ramp in flight: %v", got)
	}
	clock = now.Add(200 * time.Millisecond)
	if r.Rising() || r.Alpha("k") != 1 {
		t.Fatal("the ramp did not settle shown")
	}
	r.Poke("k")
	if got := r.Alpha("k"); got != 0 {
		t.Fatalf("a poke after the ramp did not start it over: %v", got)
	}
	r.Keep(func(string) bool { return false })
	if r.Rising() || r.Get("k") != nil {
		t.Fatal("keep did not drop the key")
	}
	r.At("j")
	f := r.Get("j")
	if got := r.Alpha("j"); got != 0 {
		t.Fatalf("a fresh key is not sunk: %v", got)
	}
	if !r.Settle() || f.Alpha(clock) != 1 || r.Settle() {
		t.Fatal("settle did not land the ramp at its target")
	}
}
