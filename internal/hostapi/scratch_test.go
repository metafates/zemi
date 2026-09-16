package hostapi

import (
	"os"
	"testing"
	"time"
)

func TestEnsureScratchKeepsResumedPad(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.agents.sessionID = "s1"
	pad := h.ScratchDir()
	old := time.Now().Add(-2 * scratchTTL)
	if err := os.MkdirAll(pad, 0o700); err != nil || os.Chtimes(pad, old, old) != nil {
		t.Fatal(err)
	}
	if h.ensureScratch() != pad {
		t.Fatal("no pad")
	}
	if _, err := os.Stat(pad); err != nil {
		t.Fatalf("a resumed session's idle pad was swept away: %v", err)
	}
}
