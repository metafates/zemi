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
// time says, and ages out a pad holding a read-only tree, which a plain
// RemoveAll cannot (a module cache copies its dirs 0555). A pad whose .root
// names a removed project goes whatever its age; a live or empty root keeps
// the TTL.
func TestScratchSweep(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	live := h.ScratchDir()
	if filepath.Base(live) != store.MainSessionID {
		t.Fatalf("pad %q is not named by the chat's id", live)
	}
	root, old := filepath.Dir(live), time.Now().Add(-2*scratchTTL)
	cases := []struct {
		name  string
		file  string
		root  *string // the pad's .root line, nil without the marker
		fresh bool
		kept  bool
		ro    bool
	}{
		{"the live pad", filepath.Join(live, "notes.md"), nil, false, true, false},
		{"a pad written deep inside", filepath.Join(root, "b", "d", "e", "out.txt"), nil, true, true, false},
		{"a pad nobody used", filepath.Join(root, "c", "d", "out.txt"), nil, false, false, false},
		{"a pad holding a read-only tree", filepath.Join(root, "d", "gomod", "out.txt"), nil, false, false, true},
		{"a pad of a removed project", filepath.Join(root, "e", "out.txt"), new(filepath.Join(t.TempDir(), "gone")), true, false, false},
		{"a pad of a live project", filepath.Join(root, "f", "out.txt"), new(t.TempDir()), true, true, false},
		{"a pad of a live project gone stale", filepath.Join(root, "g", "out.txt"), new(t.TempDir()), false, false, false},
		{"a pad whose root is empty", filepath.Join(root, "h", "out.txt"), new(""), true, true, false},
	}
	for _, c := range cases {
		if err := os.MkdirAll(filepath.Dir(c.file), 0o700); err != nil || os.WriteFile(c.file, nil, 0o600) != nil {
			t.Fatal(err)
		}
		if c.root != nil {
			mark := filepath.Join(filepath.Dir(c.file), ".root")
			if err := os.WriteFile(mark, []byte(*c.root+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if !c.fresh {
				os.Chtimes(mark, old, old)
			}
		}
		for p := c.file; p != root; p = filepath.Dir(p) {
			if !c.fresh || p != c.file {
				os.Chtimes(p, old, old)
			}
		}
		if c.ro {
			if err := os.Chmod(filepath.Dir(c.file), 0o555); err != nil {
				t.Fatal(err)
			}
		}
	}
	h.sweepScratch(root, time.Now().Add(-scratchTTL))
	for _, c := range cases {
		if _, err := os.Stat(c.file); (err == nil) != c.kept {
			t.Errorf("%s: kept %v, want %v", c.name, err == nil, c.kept)
		}
	}
	// writePadRoot is idempotent: rewriting the same root leaves the pad's
	// times alone, so an ensure cannot keep a pad fresh forever.
	h.writePadRoot(live, time.Now())
	mark := filepath.Join(live, ".root")
	padBefore, err := os.Stat(live)
	if err != nil {
		t.Fatal(err)
	}
	fileBefore, err := os.Stat(mark)
	if err != nil {
		t.Fatal(err)
	}
	padT, fileT := padBefore.ModTime(), fileBefore.ModTime()
	h.writePadRoot(live, time.Now())
	padAfter, _ := os.Stat(live)
	fileAfter, _ := os.Stat(mark)
	if !padAfter.ModTime().Equal(padT) || !fileAfter.ModTime().Equal(fileT) {
		t.Error("a second writePadRoot of the same root moved the pad's times")
	}
}

// TestScratchDeadPad: a .root naming no particular project never counts as
// dead, so a missing marker, a wiped home or the filesystem root cannot
// mass-delete pads.
func TestScratchDeadPad(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	for _, c := range []struct {
		name string
		root string // the .root line, written only when mark
		mark bool
		dead bool
	}{
		{"no marker", "", false, false},
		{"an empty marker", "", true, false},
		{"the filesystem root", "/", true, false},
		{"home", home, true, false},
		{"a removed project", filepath.Join(t.TempDir(), "gone"), true, true},
	} {
		pad := filepath.Join(t.TempDir(), "pad")
		if err := os.MkdirAll(pad, 0o700); err != nil {
			t.Fatal(err)
		}
		if c.mark {
			mark := filepath.Join(pad, ".root")
			if err := os.WriteFile(mark, []byte(c.root+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if got := deadPad(pad); got != c.dead {
			t.Errorf("%s: dead %v, want %v", c.name, got, c.dead)
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
