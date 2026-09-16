package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/sandbox"
	"github.com/metafates/zemi/internal/tools"
)

// fakeSandbox settles the probe: live or not, whatever this machine has.
func fakeSandbox(h *Host, live bool) {
	s := &sandboxProbe{backend: sandbox.Backend{Name: "fake"}}
	if !live {
		s.err = errors.New("no backend")
	}
	h.sandbox.Store(s)
}

// TestSettledCommand: builds, tests and git settle only in a trusted
// project with a live backend, where they run sandboxed; readers always.
func TestSettledCommand(t *testing.T) {
	h, _ := newTestHost(t)
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "main.go"), []byte("package main"), 0o600)
	for _, c := range []struct {
		trusted, live bool
		cmd           string
		want          bool
	}{
		{true, true, "go vet ./...", true},
		{true, false, "go vet ./...", false},
		{false, true, "go vet ./...", false},
		{false, false, "go vet ./...", false},
		{true, true, "wc -l main.go", true},
		{true, false, "wc -l main.go", true},
		{false, true, "wc -l main.go", true},
		{false, false, "wc -l main.go", true},
	} {
		h.SetProjectTrusted(c.trusted)
		fakeSandbox(h, c.live)
		if got := h.settledCommand(c.cmd); got != c.want {
			t.Errorf("trusted=%v live=%v %q: got %v", c.trusted, c.live, c.cmd, got)
		}
	}
}

// TestSandboxedBash runs the real bash tool under seatbelt: a command a
// mode settled writes the project but nothing outside it, gets the hint
// when denied, and sandbox: false goes to the user instead. Plan mode
// keeps the project read-only.
func TestSandboxedBash(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the end-to-end check drives seatbelt")
	}
	backend, err := sandbox.Probe()
	if err != nil {
		t.Skip("no sandbox:", err)
	}
	h, r := newTestHostDB(t)
	outside := t.TempDir()
	// the temp dir is writable in the sandbox, and every test dir is under it
	t.Setenv("TMPDIR", t.TempDir())
	h.sandbox.Store(&sandboxProbe{backend: backend})
	h.SetProjectTrusted(true)
	h.agents.sessionID = "s1"
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	runUser(t, r, `
		zemi.permission.mode.add("sbx", { base = "auto", rules = { { "tool.bash", "touch *", "allow" } } })
		zemi.permission.mode.add("sbxplan", { base = "plan", rules = { { "tool.bash", "touch *", "allow" } } })`)
	project := filepath.Join(h.Dirs.ProjectDir, "made.txt")
	for _, c := range []struct {
		name, mode, path string
		unsandboxed      bool
		made, hint       bool
		asked            string
	}{
		{"project write", "sbx", project, false, true, false, ""},
		{"write outside", "sbx", filepath.Join(outside, "x.txt"), false, false, true, ""},
		{"sandbox false asks", "sbx", filepath.Join(outside, "y.txt"), true, false, false, "run outside the sandbox"},
		{"plan project write", "sbxplan", filepath.Join(h.Dirs.ProjectDir, "plan.txt"), false, false, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := h.SetPermMode(c.mode); err != nil {
				t.Fatal(err)
			}
			ui.asked = nil
			in := map[string]any{"command": "touch " + c.path}
			if c.unsandboxed {
				in["sandbox"] = false
			}
			input, _ := json.Marshal(in)
			var bash fantasy.AgentTool
			for _, tl := range tools.All(h.PipelineFor()) {
				if tl.Info().Name == "bash" {
					bash = tl
				}
			}
			if _, ok := bash.Info().Parameters["sandbox"]; !ok {
				t.Fatal("bash has no sandbox parameter with a live backend")
			}
			resp, err := bash.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "bash", Input: string(input)})
			if err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(c.path)
			if made := statErr == nil; made != c.made {
				t.Errorf("made %v, want %v: %s", made, c.made, resp.Content)
			}
			if hint := strings.Contains(resp.Content, "sandbox: false"); hint != c.hint {
				t.Errorf("hint %v, want %v: %s", hint, c.hint, resp.Content)
			}
			if asked := len(ui.asked) > 0 && ui.asked[0].Reason == c.asked; asked != (c.asked != "") {
				t.Errorf("asked %+v, want reason %q", ui.asked, c.asked)
			}
		})
	}
}

// TestSandboxPolicy: the project is writable only when trusted, outside
// plan mode, with a backend that keeps .git and .zemi read-only inside it,
// when it neither holds nor sits in one of zemi's dirs and its .git is not a
// symlink; zemi's dirs are hidden unless they hold the project, and builds
// get a Go cache of their own.
func TestSandboxPolicy(t *testing.T) {
	h, _ := newTestHost(t)
	h.Dirs.StateHome = t.TempDir()
	h.Dirs.ConfigHome = t.TempDir()
	project := h.Dirs.ProjectDir
	for _, c := range []struct {
		name                   string
		trusted, plan, protect bool
		configInProject        bool
		projectInConfig        bool
		gitLink                bool
		want                   bool
	}{
		{"trusted", true, false, true, false, false, false, true},
		{"untrusted", false, false, true, false, false, false, false},
		{"plan", true, true, true, false, false, false, false},
		{"backend without protect", true, false, false, false, false, false, false},
		{"config home inside", true, false, true, true, false, false, false},
		{"inside config home", true, false, true, false, true, false, false},
		{".git is a symlink", true, false, true, false, false, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h.SetProjectTrusted(c.trusted)
			h.sandbox.Store(&sandboxProbe{backend: sandbox.Backend{Name: "fake", EnforcesProtect: c.protect}})
			dirs := h.Dirs
			defer func() { h.Dirs = dirs }()
			if c.configInProject {
				h.Dirs.ConfigHome = filepath.Join(project, ".config", "zemi")
			}
			if c.projectInConfig {
				h.Dirs.ProjectDir = filepath.Join(h.Dirs.ConfigHome, "lua")
				if err := os.MkdirAll(h.Dirs.ProjectDir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if c.gitLink {
				git := filepath.Join(project, ".git")
				if err := os.Symlink(t.TempDir(), git); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(git)
			}
			p := h.sandboxPolicy(c.plan)
			if got := slices.Contains(p.Write, h.Dirs.ProjectDir); got != c.want {
				t.Errorf("project writable %v, want %v: %v", got, c.want, p.Write)
			}
			if !slices.Contains(p.Hide, h.Dirs.StateHome) || slices.Contains(p.Hide, h.Dirs.ConfigHome) == c.projectInConfig {
				t.Errorf("hide %v", p.Hide)
			}
			cache := p.Env["GOCACHE"]
			if st, err := os.Stat(cache); err != nil || !st.IsDir() || !slices.Contains(p.Write, cache) || !strings.HasPrefix(cache, h.Dirs.StateHome) {
				t.Errorf("GOCACHE %q: %v, write %v", cache, err, p.Write)
			}
		})
	}
}
