package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
)

// TestBusy: the one-instance refusal carries the pid as its keycap, and no
// pid leaves the keycap off.
func TestBusy(t *testing.T) {
	t.Parallel()
	const text = "another zemi is already open on this project; close it to open here"
	if got := Busy(&colorprofile.Writer{}, 57356); !strings.Contains(got, " 57356 ") || !strings.Contains(got, text) {
		t.Fatalf("busy drew %q", got)
	}
	if got := Busy(&colorprofile.Writer{}, 0); strings.Contains(got, " 57356 ") {
		t.Fatalf("pidless refusal drew a keycap: %q", got)
	}
}

// TestUpdate: the parting notice names no version.
func TestUpdate(t *testing.T) {
	t.Parallel()
	got := Chip(&colorprofile.Writer{}, "update", "a new version is available; run `zemi update`")
	if !strings.Contains(got, "a new version is available; run `zemi update`") {
		t.Fatalf("update drew %q", got)
	}
	if strings.Contains(got, "v0.") {
		t.Fatalf("update named a version: %q", got)
	}
}
