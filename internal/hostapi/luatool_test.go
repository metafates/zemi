package hostapi

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/perm"
)

// TestAgentSelfAccess covers what the agent reaches of zemi itself: the lua
// tool returns prints and values without leaking globals, the scratchpad
// takes writes without asking, and memory edits apply only when autonomy
// lets them.
func TestAgentSelfAccess(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.agents.sessionID = "s1"
	ctx := context.Background()

	out, err := h.runModelLua(ctx, `print("hi", 1 + 1) leak = 1 return { n = #zemi.agent.list() }`)
	if err != nil || out != "hi\t2\nreturn: {\"n\":2}" {
		t.Fatalf("lua: %q %v", out, err)
	}
	if out, _ := h.runModelLua(ctx, `return leak == nil`); out != "return: true" {
		t.Fatalf("globals leaked between runs: %q", out)
	}

	pad := h.ScratchDir()
	if !strings.Contains(pad, filepath.Join("scratch", "s1")) {
		t.Fatalf("pad = %q", pad)
	}
	if h.fsDefault("fs.write", filepath.Join(pad, "notes.md")) != perm.Allow || h.fsDefault("fs.write", filepath.Join(h.Dirs.ProjectDir, "x")) != perm.Ask {
		t.Fatal("writes allow in the scratchpad only")
	}

	if err := h.Settings.SetHost("memory.autonomy", "auto"); err != nil {
		t.Fatal(err)
	}
	stored, id, err := h.proposeMemory("project", "fact", "the build uses make", nil)
	if err != nil || !stored {
		t.Fatalf("propose: %v %v", stored, err)
	}
	if msg, err := h.modelEditMemory(id[:8], "the build uses go build"); err != nil || !strings.HasSuffix(msg, "updated") {
		t.Fatalf("update: %q %v", msg, err)
	}
	if _, m, _ := h.locateMemory(id); m.Text != "the build uses go build" || m.Origin != learn.OriginLearned {
		t.Fatalf("text = %q origin = %q", m.Text, m.Origin)
	}
	if err := h.Settings.SetHost("memory.autonomy", "ask"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.modelEditMemory(id, ""); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("forget without auto autonomy must ask (and fail closed headless): %v", err)
	}
}
