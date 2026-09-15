package hostapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSystemPromptSections: the environment is pinned per session, project
// instructions sit under their own heading, and a profile's own prompt
// still carries the zemi section.
func TestSystemPromptSections(t *testing.T) {
	h, _ := newTestHost(t)
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "AGENTS.md"), []byte("run make test"), 0o644)
	sys := h.buildSystem(nil)
	for _, want := range []string{"# Delegating", "# Environment", "# Project instructions", "run make test", "# zemi", h.Dirs.ConfigHome} {
		if !strings.Contains(sys, want) {
			t.Errorf("default prompt misses %q", want)
		}
	}
	if env := h.environmentPrompt(); env != h.environmentPrompt() {
		t.Error("environment must be pinned")
	}
	if sys := h.buildSystem(&Profile{System: "custom"}); !strings.HasPrefix(sys, "custom") || !strings.Contains(sys, "# zemi") {
		t.Errorf("profile prompt: %.200q", sys)
	}
}
