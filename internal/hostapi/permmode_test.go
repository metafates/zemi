package hostapi

import (
	"testing"

	"github.com/metafates/zemi/internal/tools"
)

// TestPermModes: with no UI to ask, manual denies, yolo allows, and auto
// allows only what the screening model calls safe.
func TestPermModes(t *testing.T) {
	h, _ := newTestHost(t)
	ask := h.askHook()
	req := tools.Request{Subject: "tool.bash", Arg: "go test ./..."}
	for _, c := range []struct {
		mode, model string
		allowed     bool
		by          string
	}{
		{"manual", "", false, "default"},
		{"yolo", "", true, "mode:yolo"},
		{"auto", "SAFE", true, "mode:auto"},
		{"auto", "UNSAFE: deletes the home dir", false, "default"},
		{"bogus", "", false, "default"},
	} {
		h.Settings.SetHost("permissions.mode", c.mode)
		reflectStub(t, h, c.model)
		if allowed, by := ask(req); allowed != c.allowed || by != c.by {
			t.Errorf("%s/%q: allowed=%v by=%s", c.mode, c.model, allowed, by)
		}
	}
	if err := h.SetPermMode("bogus"); err == nil {
		t.Error("SetPermMode accepted an unknown mode")
	}
}
