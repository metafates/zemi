package hostapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

// TestScratchSweep: the sweep keeps the live pad however old, keeps another
// project's pad while anything in it moved within the TTL, whatever its own
// directory time says, and ages out a pad holding a read-only tree, which a
// plain RemoveAll cannot (a module cache copies its dirs 0555). The pads of
// the session-keyed root an older zemi kept go by the same rule.
func TestScratchSweep(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	live := h.ScratchDir()
	if !strings.HasPrefix(live, filepath.Join(h.Dirs.StateHome, "projects")+string(filepath.Separator)) {
		t.Fatalf("pad %q is not in a project's state dir", live)
	}
	projects := filepath.Join(h.Dirs.StateHome, "projects")
	old := filepath.Join(h.Dirs.StateHome, "scratch")
	cases := []struct {
		name  string
		pad   string
		file  string
		fresh bool
		kept  bool
		ro    bool
	}{
		{"the live pad", live, filepath.Join(live, "notes.md"), false, true, false},
		{"another project's fresh pad", filepath.Join(projects, "b", "scratch"), filepath.Join(projects, "b", "scratch", "out.txt"), true, true, false},
		{"another project's unused pad", filepath.Join(projects, "c", "scratch"), filepath.Join(projects, "c", "scratch", "out.txt"), false, false, false},
		{"a pad holding a read-only tree", filepath.Join(projects, "d", "scratch"), filepath.Join(projects, "d", "scratch", "gomod", "out.txt"), false, false, true},
		{"the pad of an old session key", filepath.Join(old, "e"), filepath.Join(old, "e", "out.txt"), false, false, false},
		{"a fresh pad of an old session key", filepath.Join(old, "f"), filepath.Join(old, "f", "out.txt"), true, true, false},
	}
	for _, c := range cases {
		if err := os.MkdirAll(filepath.Dir(c.file), 0o700); err != nil || os.WriteFile(c.file, nil, 0o600) != nil {
			t.Fatal(err)
		}
		for p := c.file; ; p = filepath.Dir(p) {
			if !c.fresh || p != c.file {
				os.Chtimes(p, time.Now().Add(-2*scratchTTL), time.Now().Add(-2*scratchTTL))
			}
			if p == c.pad {
				break
			}
		}
		if c.ro {
			if err := os.Chmod(filepath.Dir(c.file), 0o555); err != nil {
				t.Fatal(err)
			}
		}
	}
	h.sweepScratch(time.Now().Add(-scratchTTL))
	for _, c := range cases {
		if _, err := os.Stat(c.file); (err == nil) != c.kept {
			t.Errorf("%s: kept %v, want %v", c.name, err == nil, c.kept)
		}
	}
}

// TestBootSweep: the boot pass removes the orphan dirs of removed features,
// the drops idle past their TTL and the temp files a crash left, and keeps
// what is fresh.
func TestBootSweep(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.Dirs.CacheHome = t.TempDir()
	old := time.Now().Add(-2 * scratchTTL)
	cases := []struct {
		path string
		mod  time.Time
		kept bool
	}{
		{filepath.Join(h.Dirs.StateHome, "sandbox-cache", "k", "x"), old, false},
		{filepath.Join(h.Dirs.CacheHome, "lsp", "key", "gopls"), old, false},
		{filepath.Join(h.Dirs.DataHome, "site", "pack", "p"), old, false},
		{filepath.Join(h.Dirs.StateHome, "tmp", "zemi-draft.md"), old, false},
		{filepath.Join(h.Dirs.StateHome, "tmp", "zemi-fresh.md"), time.Now(), true},
		{filepath.Join(h.Dirs.StateHome, "tmp", "snap-1-x", "tree"), time.Now(), true},
		{filepath.Join(h.Dirs.DataHome, "drops", "a.png"), old, false},
		{filepath.Join(h.Dirs.DataHome, "drops", "b.png"), time.Now(), true},
	}
	for _, c := range cases {
		if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil || os.WriteFile(c.path, nil, 0o600) != nil {
			t.Fatal(err)
		}
		os.Chtimes(c.path, c.mod, c.mod)
		os.Chtimes(filepath.Dir(c.path), c.mod, c.mod)
	}
	h.SweepBoot()
	for _, c := range cases {
		if _, err := os.Stat(c.path); (err == nil) != c.kept {
			t.Errorf("%s: kept %v, want %v", c.path, err == nil, c.kept)
		}
	}
}

// TestPadBoundPerPipeline: a pipeline keeps the pad it was built with once
// the session moves on (a background run, /new), so the run reads and writes
// the pad its prompt names with no ask; another session's pad is not the
// run's to use.
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
	old, pad := h.PipelineFor(), h.ScratchDir()
	// a host tool's output, its error and a tool.post rewrite spill where the run's
	// prompt says, like shell's; a handler that misses its 50ms veto budget
	// under load leaves the output as it was, so a call may need another turn
	for _, tool := range h.luaFantasyTools(old) {
		var resp fantasy.ToolResponse
		var err error
		for range 3 {
			resp, err = tool.Run(t.Context(), fantasy.ToolCall{Input: "{}"})
			if err == nil && len(resp.Content) <= 31_000 && strings.Contains(resp.Content, "(output saved to "+pad+string(filepath.Separator)) {
				break
			}
		}
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
