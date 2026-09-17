package hostapi

import (
	"fmt"
	"strings"
	"testing"
)

// TestErrorLog: recorded errors raise the badge, the log keeps only the
// newest errLogMax, and opening /errors clears the badge.
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
	for range errLogMax + 5 {
		h.RecordError("widget chat", "render failed\x1b[31m")
	}
	if !h.HasUnseenErrors() || len(h.errs.entries) != errLogMax {
		t.Fatalf("unseen=%v entries=%d", h.HasUnseenErrors(), len(h.errs.entries))
	}
	if h.errs.entries[0].msg != "render failed" {
		t.Fatalf("message not sanitized: %q", h.errs.entries[0].msg)
	}
	h.RecordError("mcp gitlab", "initialize: connection refused")
	if err := h.OpenErrors(""); err != nil || h.HasUnseenErrors() {
		t.Fatalf("open: err=%v unseen=%v", err, h.HasUnseenErrors())
	}
	newest := ui.opened[len(ui.opened)-1].Items[0].(map[string]any)
	if !strings.Contains(newest["detail"].(string), "connection refused") || !strings.Contains(fmt.Sprint(newest["actions"]), "run:mcp restart gitlab") {
		t.Fatalf("mcp error item: %+v", newest)
	}
}
