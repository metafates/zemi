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

// TestCurtainDip: a dip dissolves the cells its words changed — the shared
// text stands, the changed span sinks with the old words and rises with the
// new ones once it lands, both over the dip's own speed.
func TestCurtainDip(t *testing.T) {
	now := time.UnixMilli(0)
	old := [][]hostapi.Segment{{{Text: "1k ctx", HL: "Dim"}}}
	next := [][]hostapi.Segment{{{Text: "2k ctx", HL: "Dim"}}}
	c := NewCurtain(100 * time.Millisecond)
	if got, _ := c.Draw(old, now); got == nil || got[0][0].Fade != 1 {
		t.Fatal("the first draw is not sunk")
	}
	if got, _ := c.Draw(old, now.Add(2*c.Dur)); got == nil || got[0][0].Fade != 0 {
		t.Fatal("the risen curtain did not draw its rows clean")
	}

	dip := 80 * time.Millisecond
	c.Dip(dip, now.Add(2*c.Dur))
	if got, _ := c.Draw(next, now.Add(2*c.Dur)); got == nil || got[0][0].Text != "1k ctx" || got[0][0].Fade != 0 {
		t.Fatalf("the dip's sink did not draw the old words whole: %v", got)
	}
	if got, _ := c.Draw(next, now.Add(2*c.Dur).Add(dip/2)); got == nil ||
		got[0][0].Text != "1" || got[0][0].Fade < 0.4 || got[0][0].Fade > 0.6 ||
		got[0][1].Text != "k ctx" || got[0][1].Fade != 0 {
		t.Fatalf("the dip's sink did not dissolve the changed cell alone: %v", got)
	}
	if got, _ := c.Draw(next, now.Add(2*c.Dur).Add(dip)); got == nil || got[0][0].Text != "2" || got[0][0].Fade != 1 {
		t.Fatalf("the dip did not hand over to the new words: %v", got)
	}
	if got, _ := c.Draw(next, now.Add(2*c.Dur).Add(dip+dip/2)); got == nil || got[0][0].Fade < 0.4 || got[0][0].Fade > 0.6 {
		t.Fatalf("the new words did not ride the rise: %v", got)
	}
	if got, _ := c.Draw(next, now.Add(2*c.Dur).Add(2*dip)); got == nil || got[0][0].Fade != 0 {
		t.Fatal("the dipped curtain did not settle clean")
	}
	if c.fade.Dur != c.Dur {
		t.Fatalf("the dip left the curtain's ramp at %v", c.fade.Dur)
	}

	// a row both draws share stands while the changed one dissolves
	shared := NewCurtain(100 * time.Millisecond)
	twoOld := [][]hostapi.Segment{{{Text: "same"}}, {{Text: "1k ctx"}}}
	twoNext := [][]hostapi.Segment{{{Text: "same"}}, {{Text: "2k ctx"}}}
	shared.Draw(twoOld, now)
	shared.Draw(twoOld, now.Add(2*c.Dur))
	shared.Dip(dip, now.Add(2*c.Dur))
	if got, _ := shared.Draw(twoNext, now.Add(2*c.Dur).Add(dip/2)); got == nil ||
		got[0][0].Text != "same" || got[0][0].Fade != 0 ||
		got[1][0].Fade < 0.4 || got[1][0].Fade > 0.6 {
		t.Fatalf("the shared row did not stand while the other dissolved: %v", got)
	}

	// a dot between two changed digits stands: the dip dissolves the runes
	// that changed, not the whole span between them
	price := NewCurtain(100 * time.Millisecond)
	priceOld := [][]hostapi.Segment{{{Text: "$55.4"}}}
	priceNext := [][]hostapi.Segment{{{Text: "$56.3"}}}
	price.Draw(priceOld, now)
	price.Draw(priceOld, now.Add(2*c.Dur))
	price.Dip(dip, now.Add(2*c.Dur))
	if got, _ := price.Draw(priceNext, now.Add(2*c.Dur).Add(dip/2)); got == nil ||
		got[0][0].Text != "$5" || got[0][0].Fade != 0 ||
		got[0][1].Text != "5" || got[0][1].Fade < 0.4 || got[0][1].Fade > 0.6 ||
		got[0][2].Text != "." || got[0][2].Fade != 0 ||
		got[0][3].Text != "4" || got[0][3].Fade < 0.4 || got[0][3].Fade > 0.6 {
		t.Fatalf("the dot between two changed digits did not stand: %v", got)
	}

	// a dip on a hidden curtain does nothing
	c.Draw(nil, now.Add(9*c.Dur))
	if got, _ := c.Draw(nil, now.Add(9*c.Dur).Add(c.Dur)); got != nil {
		t.Fatal("the sink did not ride out before the second dip")
	}
	c.Dip(dip, now.Add(9*c.Dur).Add(c.Dur))
	if got, _ := c.Draw(nil, now.Add(9*c.Dur).Add(c.Dur)); got != nil {
		t.Fatal("a dip on a hidden curtain sank rows")
	}
}

// TestSpread: rows a ramp already carries ride it, growth past it joins the
// newest ramp while it rises or starts the next one, and Retain drops the
// keys keep refuses.
func TestSpread(t *testing.T) {
	clock := time.UnixMilli(0)
	r := NewRise[string](100*time.Millisecond, func() time.Time { return clock })
	if got := r.Spread("call", 0); got != 0 {
		t.Fatalf("a fresh spread is not sunk: %v", got)
	}
	clock = clock.Add(50 * time.Millisecond)
	if got := r.Spread("call", 0); got < 0.4 || got > 0.6 {
		t.Fatalf("the stamped ramp did not ride: %v", got)
	}
	// a row past the ramp's rows joins it while it rises
	if got := r.Spread("call", 1); got != r.Alpha("call") {
		t.Fatalf("the grown row did not ride the ramp: %v vs %v", got, r.Alpha("call"))
	}
	// once it settles, the next row starts a new ramp
	clock = clock.Add(10 * r.Dur)
	if got := r.Spread("call", 2); got != 0 {
		t.Fatalf("a row past a settled ramp did not start sunk: %v", got)
	}
	if got := r.Spread("call", 1); got != 1 {
		t.Fatalf("the settled ramp's rows did not stay risen: %v", got)
	}
	r.Retain(func(id string) bool { return id != "call" })
	if _, ok := r.ramps["call"]; ok {
		t.Fatal("retain kept a refused key's ramps")
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
	if _, ok := r.ramps["sunk"]; ok {
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
