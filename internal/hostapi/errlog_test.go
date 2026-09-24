package hostapi

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestErrorLog: recorded errors raise the badge, the log keeps only the
// newest errLogMax, keeps a repeat once at its later time, and opening /errors clears the badge.
func TestErrorLog(t *testing.T) {
	h, _ := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.OpenErrors(""); err != nil || len(ui.opened) != 1 || ui.opened[0].Empty == "" {
		t.Fatalf("no errors should open the empty list: %v %+v", err, ui.opened)
	}
	if h.HasUnseenErrors() {
		t.Fatal("fresh host reports errors")
	}
	for i := range errLogMax + 5 {
		h.RecordError("widget chat", fmt.Sprintf("render failed %d\x1b[31m", i))
	}
	h.errs.entries[errLogMax-1].at = time.Time{}
	h.RecordError("toast", fmt.Sprintf("render failed %d", errLogMax+4))
	if !h.HasUnseenErrors() || len(h.errs.entries) != errLogMax || h.errs.entries[errLogMax-1].at.IsZero() {
		t.Fatalf("unseen=%v entries=%d", h.HasUnseenErrors(), len(h.errs.entries))
	}
	if h.errs.entries[0].msg != "render failed 5" {
		t.Fatalf("message not sanitized: %q", h.errs.entries[0].msg)
	}
	h.RecordError("mcp gitlab", "initialize: connection refused")
	if err := h.OpenErrors(""); err != nil || h.HasUnseenErrors() {
		t.Fatalf("open: err=%v unseen=%v", err, h.HasUnseenErrors())
	}
	newest := ui.opened[len(ui.opened)-1].Items[0].(map[string]any)
	if !strings.Contains(newest["detail"].(string), "\nerror: initialize: connection refused") || !strings.Contains(fmt.Sprint(newest["actions"]), "hotkey:r run:mcp restart gitlab") {
		t.Fatalf("mcp error item: %+v", newest)
	}
}
