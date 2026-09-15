package hostapi

import "testing"

// TestErrorLog: recorded errors raise the badge, the log keeps only the
// newest errLogMax, and opening /errors clears the badge.
func TestErrorLog(t *testing.T) {
	h, _ := newTestHost(t)
	h.SetUIHooks(&fakeUI{})
	if h.HasUnseenErrors() {
		t.Fatal("fresh host reports errors")
	}
	for range errLogMax + 5 {
		h.RecordError("widget chat", "render failed\x1b[31m")
	}
	if !h.HasUnseenErrors() || len(h.errs.entries) != errLogMax {
		t.Fatalf("unseen=%v entries=%d", h.HasUnseenErrors(), len(h.errs.entries))
	}
	if h.errs.entries[0].msg != "render failed" {
		t.Fatalf("message not sanitized: %q", h.errs.entries[0].msg)
	}
	if err := h.OpenErrors(""); err != nil || h.HasUnseenErrors() {
		t.Fatalf("open: err=%v unseen=%v", err, h.HasUnseenErrors())
	}
}
