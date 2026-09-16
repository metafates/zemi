package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
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
// mode settled writes the project and its own temp dir but nothing outside
// them, the system temp dir included (xcrun_db there picks what /usr/bin/make
// runs), gets the hint when denied, and sandbox: false goes to the user
// instead. Plan mode keeps the project read-only, and go test still passes.
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
	// a probe beside xcrun_db, not the file itself: the rule is for the dir
	shared := filepath.Join(os.TempDir(), fmt.Sprintf("zemi-sandbox-probe-%d", os.Getpid()))
	t.Cleanup(func() { os.Remove(shared) })
	h.sandbox.Store(&sandboxProbe{backend: backend})
	h.SetProjectTrusted(true)
	h.agents.sessionID = "s1"
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	runUser(t, r, `
		zemi.permission.mode.add("sbx", { base = "auto", rules = { { "tool.bash", "touch *", "allow" } } })
		zemi.permission.mode.add("sbxplan", { base = "plan", rules = { { "tool.bash", "touch *", "allow" } } })`)
	project := filepath.Join(h.Dirs.ProjectDir, "made.txt")
	venv := filepath.Join(h.Dirs.ProjectDir, ".venv", "bin")
	os.MkdirAll(venv, 0o755)
	t.Setenv("PATH", venv+string(filepath.ListSeparator)+os.Getenv("PATH"))
	private := filepath.Join(h.sandboxPolicy(false).Env["TMPDIR"], "made.txt")
	run := func(in map[string]any) fantasy.ToolResponse {
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
		return resp
	}
	for _, c := range []struct {
		name, mode, path string
		unsandboxed      bool
		made, hint       bool
		asked            string
	}{
		{"project write", "sbx", project, false, true, false, ""},
		{"private temp write", "sbx", private, false, true, false, ""},
		{"binary planted in a PATH dir", "sbx", filepath.Join(venv, "git"), false, false, true, ""},
		{"system temp write", "sbx", shared, false, false, true, ""},
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
			resp := run(in)
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
	t.Run("go test", func(t *testing.T) {
		if err := h.SetPermMode("auto"); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "go.mod"), []byte("module m\n\ngo 1.22\n"), 0o644)
		os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "m_test.go"), []byte("package m\n\nimport \"testing\"\n\nfunc TestTemp(t *testing.T) { t.TempDir() }\n"), 0o644)
		ui.asked = nil
		if resp := run(map[string]any{"command": "cd " + h.Dirs.ProjectDir + " && go test -count=1 ./...", "timeout_ms": 300000.0}); resp.IsError || !strings.Contains(resp.Content, "ok") || len(ui.asked) > 0 {
			t.Errorf("go test in the sandbox: %s, asked %+v", resp.Content, ui.asked)
		}
	})
}

// TestSandboxPolicy: the project is writable only when trusted, outside
// plan mode, with a backend that keeps .git and .zemi read-only inside it,
// when it neither holds nor sits in one of zemi's dirs, holds no hidden path
// on a backend that cannot hide inside a write root, and its .git is not a
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
		secretInside           string // backend name when the project is ~/.config
		want                   bool
	}{
		{"trusted", true, false, true, false, false, false, "", true},
		{"untrusted", false, false, true, false, false, false, "", false},
		{"plan", true, true, true, false, false, false, "", false},
		{"backend without protect", true, false, false, false, false, false, "", false},
		{"config home inside", true, false, true, true, false, false, "", false},
		{"inside config home", true, false, true, false, true, false, "", false},
		{".git is a symlink", true, false, true, false, false, true, "", false},
		{"holds ~/.config/gh under landlock", true, false, true, false, false, false, "fake", false},
		{"holds ~/.config/gh under seatbelt", true, false, true, false, false, false, "seatbelt", true},
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
			if c.secretInside != "" {
				h.sandbox.Store(&sandboxProbe{backend: sandbox.Backend{Name: c.secretInside, EnforcesProtect: true}})
				home := t.TempDir()
				t.Setenv("HOME", home)
				h.Dirs.ProjectDir = filepath.Join(home, ".config")
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
			for _, dir := range []string{p.Env["GOCACHE"], p.Env["TMPDIR"]} {
				if st, err := os.Stat(dir); err != nil || !st.IsDir() || !slices.Contains(p.Write, dir) || !strings.HasPrefix(dir, h.Dirs.StateHome) {
					t.Errorf("GOCACHE or TMPDIR %q: %v, write %v", dir, err, p.Write)
				}
			}
			if slices.Contains(p.Write, os.TempDir()) {
				t.Errorf("the shared temp dir is writable: %v", p.Write)
			}
		})
	}
	venv := filepath.Join(project, ".venv", "bin")
	for _, c := range []struct {
		name, path        string
		writable, protect bool
	}{
		{"PATH dir inside the project", venv + string(filepath.ListSeparator) + "/usr/bin", true, true},
		{"relative PATH dir", "bin" + string(filepath.ListSeparator) + "/usr/bin", false, false},
	} {
		t.Setenv("PATH", c.path)
		p := h.sandboxPolicy(false)
		if got := slices.Contains(p.Write, project); got != c.writable {
			t.Errorf("%s: project writable %v: %v", c.name, got, p.Write)
		}
		if got := slices.Contains(p.Protect, venv); got != c.protect {
			t.Errorf("%s: %s protected %v: %v", c.name, venv, got, p.Protect)
		}
	}
	h.Dirs.CacheHome = t.TempDir()
	server := h.serverPolicy("rust-analyzer")
	dir := filepath.Join(h.Dirs.CacheHome, "lsp", h.trustKey(), "rust-analyzer")
	if !slices.Equal(server.Write, []string{dir}) || !server.Loopback || !slices.Contains(server.Hide, h.Dirs.StateHome) {
		t.Errorf("server policy %+v", server)
	}
	for _, k := range []string{"CARGO_TARGET_DIR", "GOCACHE", "XDG_CACHE_HOME", "TMPDIR"} {
		if st, err := os.Stat(server.Env[k]); err != nil || !st.IsDir() || !strings.HasPrefix(server.Env[k], dir) {
			t.Errorf("server %s %q: %v", k, server.Env[k], err)
		}
	}
	a := h.sandboxPolicy(false).Env["GOCACHE"]
	h.Dirs.ProjectDir = t.TempDir()
	if b := h.sandboxPolicy(false).Env["GOCACHE"]; a == b {
		t.Errorf("two trust roots share the Go cache %q", a)
	}
}

// TestSandboxGuard: a protected path missing when a sandboxed command
// started and there once it ran (the namespace backend binds only existing
// ones) holds trust off without saving, noticed at exit or while it runs,
// and the result names it.
func TestSandboxGuard(t *testing.T) {
	for _, running := range []bool{false, true} {
		h, _ := newTestHost(t)
		h.Dirs.StateHome = t.TempDir()
		h.SetProjectTrusted(true)
		h.sandbox.Store(&sandboxProbe{backend: sandbox.Backend{Name: "fake", EnforcesProtect: true}})
		done := h.guardSandbox(h.sandboxPolicy(false))
		// what `mkdir .zemi && echo 'print(1)' > .zemi/init.lua` leaves behind
		zemiDir := filepath.Join(h.Dirs.ProjectDir, ".zemi")
		if err := os.MkdirAll(zemiDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(zemiDir, "init.lua"), []byte("print(1)"), 0o644); err != nil {
			t.Fatal(err)
		}
		if running {
			h.CheckSandboxed()
			if h.ProjectTrusted() {
				t.Error("running: still trusted")
			}
		}
		note := done()
		if h.ProjectTrusted() || !h.TrustHeld() || !strings.Contains(note, zemiDir) || !strings.Contains(note, "/trust") {
			t.Errorf("running %v: trusted %v held %v note %q", running, h.ProjectTrusted(), h.TrustHeld(), note)
		}
	}
	h, _ := newTestHost(t)
	h.SetProjectTrusted(true)
	h.sandbox.Store(&sandboxProbe{backend: sandbox.Backend{Name: "fake", EnforcesProtect: true}})
	if note := h.guardSandbox(h.sandboxPolicy(false))(); note != "" || !h.ProjectTrusted() {
		t.Errorf("nothing created: note %q trusted %v", note, h.ProjectTrusted())
	}
}

// TestSandboxNestedConfig: on a backend that protects only the paths it
// binds, a .git or .zemi a sandboxed command creates anywhere in the project
// holds trust off, one already there is protected, and a project too big to
// walk holds trust off at exit or stays read-only.
func TestSandboxNestedConfig(t *testing.T) {
	defer func(n int) { sandboxWalkMax = n }(sandboxWalkMax)
	for _, c := range []struct {
		name          string
		before, after string // files there when the command starts, and created by it
		exitMax       int    // sandboxWalkMax once the command runs, 0 unchanged
		held          string // in the note when trust is held
	}{
		{"nested .git dir", "", "pkg/.git/config", 0, filepath.Join("pkg", ".git")},
		{"nested .git file", "", "pkg/.git", 0, filepath.Join("pkg", ".git")},
		{"nested .zemi", "", "a/b/.zemi/init.lua", 0, filepath.Join("b", ".zemi")},
		{"plain file", "", "pkg/main.go", 0, ""},
		{"existing nested repo", "vendor/x/.git/config", "", 0, ""},
		{"too many entries at exit", "", "pkg/main.go", 1, "too many"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sandboxWalkMax = 100_000
			h, _ := newTestHost(t)
			h.Dirs.StateHome = t.TempDir()
			h.SetProjectTrusted(true)
			h.sandbox.Store(&sandboxProbe{backend: sandbox.Backend{Name: "fake", EnforcesProtect: true}})
			touch := func(rel string) {
				p := filepath.Join(h.Dirs.ProjectDir, rel)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("gitdir: ../elsewhere"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if c.before != "" {
				touch(c.before)
			}
			p := h.sandboxPolicy(false)
			if !slices.Contains(p.Write, h.Dirs.ProjectDir) {
				t.Fatalf("project not writable: %v", p.Write)
			}
			if c.before != "" && !slices.Contains(p.Protect, filepath.Join(h.Dirs.ProjectDir, filepath.Dir(c.before))) {
				t.Errorf("existing entry not protected: %v", p.Protect)
			}
			done := h.guardSandbox(p)
			if c.after != "" {
				touch(c.after)
			}
			if c.exitMax > 0 {
				sandboxWalkMax = c.exitMax
			}
			note := done()
			if held := !h.ProjectTrusted(); held != (c.held != "") || !strings.Contains(note, c.held) {
				t.Errorf("held %v, note %q", held, note)
			}
		})
	}
	sandboxWalkMax = 1
	h, _ := newTestHost(t)
	h.SetProjectTrusted(true)
	h.sandbox.Store(&sandboxProbe{backend: sandbox.Backend{Name: "fake", EnforcesProtect: true}})
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "main.go"), nil, 0o644)
	if p := h.sandboxPolicy(false); slices.Contains(p.Write, h.Dirs.ProjectDir) {
		t.Errorf("a project too big to walk is writable: %v", p.Write)
	}
}

func TestTrustUnsaved(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	for _, c := range []struct {
		name, root string
		want       bool
	}{
		{"home", home, true},
		{"filesystem root", "/", true},
		{"none", "", true},
		{"repo below home", filepath.Join(home, "repo"), false},
		{"home in upper case", strings.ToUpper(home), perm.FoldCase("A") != "A"},
	} {
		if got := TrustUnsaved(c.root); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
