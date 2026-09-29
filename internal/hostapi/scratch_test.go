package hostapi

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// TestScratchSweep: the sweep keeps the live pad however old, and another
// pad while anything in it moved within the TTL, whatever its own directory
// time says.
func TestScratchSweep(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.agents.sessionID = store.NewID()
	live := h.ScratchDir()
	if len(filepath.Base(live)) != padIDLen {
		t.Fatalf("pad %q is not named by %d characters of the id", live, padIDLen)
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
// another session's pad is zemi's own files to anyone else.
func TestPadBoundPerPipeline(t *testing.T) {
	h, r := newTestHostDB(t)
	fakeSandbox(h, true)
	h.SetProjectTrusted(true)
	h.Perm.Add(perm.Rule{Subject: "tool.long", Pattern: "*", Decision: perm.Allow})
	h.Perm.Add(perm.Rule{Subject: "tool.short", Pattern: "*", Decision: perm.Allow})
	h.Perm.Add(perm.Rule{Subject: "tool.failing", Pattern: "*", Decision: perm.Allow})
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
	if resp, err := h.luaEvalTool(old).Run(t.Context(), fantasy.ToolCall{Input: `{"description":"d","path":"` + script + `"}`}); err != nil || resp.Content != "ran" {
		t.Errorf("a lua script in the bound pad: %q %v", resp.Content, err)
	}
	for _, c := range []struct {
		name, subject string
		p             *tools.Pipeline
		want          perm.Decision
		why           string
	}{
		{"write the bound pad", perm.SubjectWrite, old, perm.Allow, ""},
		{"read the bound pad", perm.SubjectRead, old, perm.Allow, ""},
		{"write another session's pad", perm.SubjectWrite, h.PipelineFor(), perm.Deny, "zemi's own files"},
	} {
		v := c.p.Check(tools.Request{Subject: c.subject, Arg: filepath.Join(pad, "notes.md")})
		if v.Decision != c.want || !strings.Contains(v.Reason, c.why) {
			t.Errorf("%s: %s (%s), want %s", c.name, v.Decision, v.Reason, c.want)
		}
	}
	pol := old.SandboxFor(tools.Request{Subject: perm.SubjectShell, Arg: "ls"}, tools.Verdict{Decision: perm.Allow, DecidedBy: tools.ByMode("auto")})
	if pol == nil || pol.Env["TMPDIR"] != pad || !slices.Contains(pol.Write, pad) {
		t.Errorf("the run's sandbox lost its pad %s: %+v", pad, pol)
	}
}
