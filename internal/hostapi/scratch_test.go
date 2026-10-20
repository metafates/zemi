package hostapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/store"
)

// TestScratchSweep: the sweep keeps the live pad however old, and another
// pad while anything in it moved within the TTL, whatever its own directory
// time says.
func TestScratchSweep(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.agents.sessionID = store.ShortID()
	live := h.ScratchDir()
	if filepath.Base(live) != h.agents.sessionID {
		t.Fatalf("pad %q is not named by the session id", live)
	}
	root, old := filepath.Dir(live), time.Now().Add(-2*scratchTTL)
	cases := []struct {
		name, file  string
		fresh, kept bool
	}{
		{"the live pad", filepath.Join(live, "notes.md"), false, true},
		{"a pad written deep inside", filepath.Join(root, "b", "d", "e", "out.txt"), true, true},
		{"a pad nobody used", filepath.Join(root, "c", "d", "out.txt"), false, false},
	}
	for _, c := range cases {
		if err := os.MkdirAll(filepath.Dir(c.file), 0o700); err != nil || os.WriteFile(c.file, nil, 0o600) != nil {
			t.Fatal(err)
		}
		for p := c.file; p != root; p = filepath.Dir(p) {
			if !c.fresh || p != c.file {
				os.Chtimes(p, old, old)
			}
		}
	}
	h.sweepScratch(root, time.Now().Add(-scratchTTL))
	for _, c := range cases {
		if _, err := os.Stat(c.file); (err == nil) != c.kept {
			t.Errorf("%s: kept %v, want %v", c.name, err == nil, c.kept)
		}
	}
}

// TestPadBoundPerPipeline: a pipeline keeps the pad it was built with once
// the session moves on (a background run, /new), so the run reads and writes
// the pad its prompt names with no ask and its commands get it as $TMPDIR;
// another session's pad is not the run's to use.
func TestPadBoundPerPipeline(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	h.SetProjectTrusted(true)
	runUser(t, r, `
		zemi.tool.add({ name = "long", description = "d", execute = function() return string.rep("x", 40000) end })
		zemi.tool.add({ name = "short", description = "d", execute = function() return "ok" end })
		zemi.tool.add({ name = "failing", description = "d", execute = function() return nil, string.rep("x", 40000) end })
		zemi.event.on("tool.post", function(ev)
			if ev.tool == "short" then return { output = string.rep("x", 40000) } end
		end)`)
	h.agents.sessionID = "s1"
	old, pad := h.PipelineFor(), h.ScratchDir()
	h.agents.sessionID = "s2"
	// a host tool's output, its error and a tool.post rewrite spill where the run's
	// prompt says, like shell's
	for _, tool := range h.luaFantasyTools(old) {
		resp, err := tool.Run(t.Context(), fantasy.ToolCall{Input: "{}"})
		if err != nil || resp.IsError != (tool.Info().Name == "failing") || len(resp.Content) > 31_000 || !strings.Contains(resp.Content, "(output saved to "+pad+string(filepath.Separator)) {
			t.Errorf("%s: %d bytes, spill not in the bound pad %s: %v %q", tool.Info().Name, len(resp.Content), pad, err, resp.Content[max(0, len(resp.Content)-200):])
		}
	}
	script := filepath.Join(pad, "x.lua")
	if err := os.MkdirAll(pad, 0o700); err != nil || os.WriteFile(script, []byte(`print("ran")`), 0o600) != nil {
		t.Fatal(err)
	}
	if resp, err := h.luaEvalTool(old).Run(t.Context(), fantasy.ToolCall{Input: `{"about":"d","path":"` + script + `"}`}); err != nil || resp.Content != "ran" {
		t.Errorf("a lua script in the bound pad: %q %v", resp.Content, err)
	}
}
