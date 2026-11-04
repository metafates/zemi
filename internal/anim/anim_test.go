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
}
