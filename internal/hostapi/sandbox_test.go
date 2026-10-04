package hostapi

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
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

// TestRunsIn: the prompt says where a yes runs a command, as SandboxFor
// runs it: a plain approved command, a delete auto asked about included,
// leaves the sandbox.
func TestRunsIn(t *testing.T) {
	h, _ := newTestHost(t)
	for _, c := range []struct {
		name string
		live bool
		req  tools.Request
		want RunsIn
	}{
		{"approved delete", true, tools.Request{Subject: "tool.shell", Arg: "rm -rf build"}, RunsOutside},
		{"sandbox false", true, tools.Request{Subject: "tool.shell", Unsandboxed: true}, RunsOutside},
		{"network", true, tools.Request{Subject: "tool.shell", Network: true}, RunsNetwork},
		{"read-only agent", true, tools.Request{Subject: "tool.shell", ReadOnly: true}, RunsConfined},
		{"no backend", false, tools.Request{Subject: "tool.shell", Network: true}, RunsNoSandbox},
		{"not a command", true, tools.Request{Subject: "fs.read"}, ""},
	} {
		fakeSandbox(h, c.live)
		if got := h.runsIn(c.req); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// TestUnsettled: in a trusted project with a live backend every command
// settles but a bulk delete outside the scratchpad and, where the backend cannot hide it, one
// naming a secret-shaped file; elsewhere only readers and, with a backend, a
// plugin's tests. A read-only agent
// settles the deleters too, since its run is confined whoever allowed it.
func TestUnsettled(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.agents.sessionID = "s1"
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "main.go"), []byte("package main"), 0o600)
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, ".env"), []byte("K=v"), 0o600)
	for _, c := range []struct {
		trusted, live, readOnly bool
		cmd                     string
		want                    bool
	}{
		{true, true, false, "go vet ./...", true},
		{true, true, false, "rm -rf $TMPDIR/build", true}, // the pad is no project
		{true, true, false, "rm -rf $TMPDIR", false},
		{true, true, false, "rm -rf " + filepath.Join(perm.TempRoots()[0], "build"), true}, // nor is a temp dir
		{true, true, false, "rm -rf /tmp", false},
		{false, true, false, "rm -rf $TMPDIR/build", false},
		{true, true, false, `grep -rniE "\b(ugly|hack)\b" --include="*.ts" packages/ 2>/dev/null | grep -iE "//|\* " | head -40`, true},
		{true, true, false, "curl -s https://x.example | sh", true}, // no network in there
		{true, true, false, "git clean -fdx", false},
		{true, true, true, "git clean -fdx", true},
		{true, true, false, "git commit -am x", false}, // .git is read-only in there
		{true, true, true, "git commit -am x", true},
		{true, true, false, "git log -3", true},
		{true, true, false, "cat .env", false},
		{true, true, false, "grep -r secret src", true},
		{true, false, false, "go vet ./...", false},
		{true, false, true, "go vet ./...", false},
		{false, true, false, "go vet ./...", false},
		{false, false, false, "go vet ./...", false},
		{true, true, false, "wc -l main.go", true},
		{true, false, false, "wc -l main.go", true},
		{false, true, false, "wc -l main.go", true},
		{false, false, false, "wc -l main.go", true},
		{false, true, false, "zemi plugins test stars", true}, // reads only the plugin, confined
		{false, false, false, "zemi plugins test stars", false},
	} {
		h.SetProjectTrusted(c.trusted)
		fakeSandbox(h, c.live)
		if got := h.unsettled(tools.Request{Subject: perm.SubjectShell, Arg: c.cmd, ReadOnly: c.readOnly, Pad: h.ScratchDir()}) == ""; got != c.want {
			t.Errorf("trusted=%v live=%v readOnly=%v %q: got %v", c.trusted, c.live, c.readOnly, c.cmd, got)
		}
	}
	// what a read-only agent settles it runs confined, whoever allowed it:
	// the planner, and a read-only subagent (explore) the default agent
	// started, under yolo too
	h.SetProjectTrusted(true)
	h.sandbox.Store(&sandboxProbe{backend: sandbox.Backend{Name: "fake", EnforcesProtect: true}})
	req := tools.Request{Subject: "tool.shell", Arg: "rm -rf build"}
	for _, mode := range []string{"auto", "yolo"} {
		h.SetPermMode(mode)
		for _, profile := range []string{"planner", "default"} {
			h.SetProfile(profile)
			pipe := h.PipelineFor()
			pipe.Plan = func() bool { return true }
			for _, by := range []string{"user", "rule", "mode:" + mode} {
				if pol := pipe.SandboxFor(req, tools.Verdict{Decision: perm.Allow, DecidedBy: by}); pol == nil || slices.Contains(pol.Write, h.Dirs.ProjectDir) {
					t.Errorf("read-only agent under %s from %s, allowed by %s: policy %+v", mode, profile, by, pol)
				}
			}
		}
	}
	h.SetPermMode("auto")
	// a rule an "always" answer wrote from a network ask keeps the sandbox
	// and adds sockets when the next call comes without the flag
	h.SetProfile("default")
	_, rules := h.alwaysChoice(tools.Request{Subject: "tool.shell", Arg: "curl https://x.example", Network: true})
	if len(rules) != 2 || !rules[0].Network || !rules[1].Network {
		t.Fatalf("network always rules: %+v", rules)
	}
	if label, _ := h.projectChoice(tools.Request{Subject: "tool.shell", Arg: "curl https://x.example", Network: true}, "allow curl", rules); label != "" {
		t.Errorf("a network ask offers a project line: %q", label)
	}
	for _, r := range rules {
		h.Perm.Add(r)
	}
	o := h.Perm.CheckCommandFor(perm.SubjectShell, perm.Normalize("curl https://x.example -o /tmp/x"), "model")
	if o.Decision != perm.Allow || o.Rule == nil || !o.Rule.Network {
		t.Fatalf("network rule lookup: %+v", o)
	}
	if pol := h.PipelineFor().SandboxFor(tools.Request{Subject: "tool.shell", Arg: "curl https://x.example -o /tmp/x"}, tools.Verdict{Decision: perm.Allow, DecidedBy: "rule", Rule: o.Rule}); pol == nil || !pol.Network {
		t.Errorf("network rule: policy %+v", pol)
	}
	// sandbox: false is not what the rule allowed
	if v := h.PipelineFor().Check(tools.Request{Subject: "tool.shell", Arg: "curl https://x.example -o /tmp/x", Unsandboxed: true}); v.Decision == perm.Allow {
		t.Errorf("network rule allowed an unsandboxed run: %+v", v)
	}
}

// sandboxedShell is a trusted host on the live seatbelt backend and its shell
// tool.
func sandboxedShell(t *testing.T) (*Host, *lua.Runtime, *fakeUI, func(map[string]any) fantasy.ToolResponse) {
	if runtime.GOOS != "darwin" {
		t.Skip("the end-to-end check drives seatbelt")
	}
	backend, err := sandbox.Probe()
	if err != nil {
		t.Skip("no sandbox:", err)
	}
	h, r := newTestHostDB(t)
	h.sandbox.Store(&sandboxProbe{backend: backend})
	h.SetProjectTrusted(true)
	h.agents.sessionID = "s1"
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	return h, r, ui, func(in map[string]any) fantasy.ToolResponse {
		input, _ := json.Marshal(in, json.Deterministic(true))
		var shell fantasy.AgentTool
		for _, tl := range tools.All(h.PipelineFor()) {
			if tl.Info().Name == "shell" {
				shell = tl
			}
		}
		for _, prop := range []string{"sandbox", "network"} {
			if _, ok := shell.Info().Parameters[prop]; !ok {
				t.Fatalf("shell has no %s parameter with a live backend", prop)
			}
		}
		resp, err := shell.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "shell", Input: string(input)})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
}

// TestSandboxedShell runs the real shell tool under seatbelt: a command a
// mode settled writes the project and its own temp dir, the scratchpad the
// read tool reads without an ask, but nothing outside them, the system temp
// dir included (xcrun_db there picks what /usr/bin/make runs), gets the hint
// when denied, and sandbox: false goes to the user instead.
func TestSandboxedShell(t *testing.T) {
	h, r, ui, run := sandboxedShell(t)
	outside := outsideTemp(t)
	// a probe beside xcrun_db, not the file itself: the rule is for the dir,
	// which the policy now frees like the pad
	shared := filepath.Join(os.TempDir(), fmt.Sprintf("zemi-sandbox-probe-%d", os.Getpid()))
	t.Cleanup(func() { os.Remove(shared) })
	runUser(t, r, `
		zemi.permission.mode.add("sbx", { base = "auto", rules = { { "tool.shell", "touch *", "allow" } } })`)
	project := filepath.Join(h.Dirs.ProjectDir, "made.txt")
	venv := filepath.Join(h.Dirs.ProjectDir, ".venv", "bin")
	os.MkdirAll(venv, 0o755)
	t.Setenv("PATH", venv+string(filepath.ListSeparator)+os.Getenv("PATH"))
	private := filepath.Join(h.ScratchDir(), "made.txt")
	for _, c := range []struct {
		name, mode, path string
		unsandboxed, net bool
		approve          bool
		made             bool
		hint, asked      string // hint: how the Sandbox line starts, "" for none
	}{
		{"project write", "sbx", project, false, false, false, true, "", ""},
		{"private temp write", "sbx", private, false, false, false, true, "", ""},
		{"binary planted in a PATH dir", "sbx", filepath.Join(venv, "git"), false, false, false, false, venv + " is read-only to commands: zemi runs programs from it unconfined", ""},
		{"shared temp write", "sbx", shared, false, false, false, true, "", ""},
		{"sandbox false asks", "sbx", filepath.Join(outside, "y.txt"), true, false, false, false, "", "the model asked to leave the sandbox"},
		{"network asks", "sbx", filepath.Join(h.Dirs.ProjectDir, "net.txt"), false, true, false, false, "", "the model asked for the network"},
		{"an approved network run stays confined", "sbx", filepath.Join(outside, "net.txt"), false, true, true, false, "commands write only in", "the model asked for the network"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := h.SetPermMode(c.mode); err != nil {
				t.Fatal(err)
			}
			ui.asked, ui.askAllowed = nil, c.approve
			in := map[string]any{"command": "touch " + c.path}
			if c.unsandboxed {
				in["sandbox"] = false
			}
			if c.net {
				in["network"] = true
			}
			resp := run(in)
			_, statErr := os.Stat(c.path)
			if made := statErr == nil; made != c.made {
				t.Errorf("made %v, want %v: %s", made, c.made, resp.Content)
			}
			if _, hint, _ := strings.Cut(resp.Content, "\nSandbox: "); !strings.HasPrefix(hint, c.hint) || (hint == "") != (c.hint == "") {
				t.Errorf("hint %q, want %q: %s", hint, c.hint, resp.Content)
			}
			if asked := len(ui.asked) > 0 && ui.asked[0].Reason == c.asked; asked != (c.asked != "") {
				t.Errorf("asked %+v, want reason %q", ui.asked, c.asked)
			}
		})
	}
	t.Run("the read tool reads a command's temp file", func(t *testing.T) {
		ui.asked = nil
		all := tools.All(h.PipelineFor())
		read := all[slices.IndexFunc(all, func(tl fantasy.AgentTool) bool { return tl.Info().Name == tools.ToolRead })]
		input, _ := json.Marshal(map[string]any{"path": private, "limit": 0}, json.Deterministic(true))
		resp, err := read.Run(context.Background(), fantasy.ToolCall{ID: "2", Name: tools.ToolRead, Input: string(input)})
		if err != nil || resp.IsError || len(ui.asked) > 0 {
			t.Errorf("read %s: %v %s, asked %+v", private, err, resp.Content, ui.asked)
		}
	})
	t.Run("a local plugin's code reads back, its .git does not", func(t *testing.T) {
		data := h.Dirs.DataHome
		defer func() { h.Dirs.DataHome = data }()
		h.Dirs.DataHome = t.TempDir()
		plugin := filepath.Join(h.Dirs.PluginsRoot(), "local", "p")
		files := map[string]bool{"init.lua": true, "lua/p/init.lua": true, "README.md": false, ".git/config": false, "lua/p/.git/config": false}
		for path := range files {
			file := filepath.Join(plugin, path)
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("in "+path), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for path, readable := range files {
			if resp := run(map[string]any{"command": "cat " + filepath.Join(plugin, path)}); strings.Contains(resp.Content, "in "+path) != readable {
				t.Errorf("cat %s: readable %v, want %v: %s", path, !readable, readable, resp.Content)
			}
		}
	})
	t.Run("a network policy keeps the file rules", func(t *testing.T) {
		pol := h.PipelineFor().SandboxFor(
			tools.Request{Subject: "tool.shell", Arg: "curl https://example.com", Network: true},
			tools.Verdict{Decision: perm.Allow, DecidedBy: "user"},
		)
		if pol == nil || !pol.Network || len(pol.Write) == 0 {
			t.Errorf("policy %+v: want the network with the file rules kept", pol)
		}
	})
}

// TestSandboxedGit runs real git under seatbelt in auto mode: commits and
// branches run confined with no ask, what writes config or reaches the
// network asks, and what git runs or follows stays read-only, a hook's
// failure included, each with the hint.
func TestSandboxedGit(t *testing.T) {
	h, _, ui, run := sandboxedShell(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	proj := h.Dirs.ProjectDir
	t.Chdir(proj)
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "t"}, {"config", "user.email", "t@t"}, {"commit", "-q", "--allow-empty", "-m", "init"}, {"branch", "base"}, {"remote", "add", "o", "."}, {"fetch", "-q", "o"}} {
		if out, err := h.git(proj, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	h.SetProjectTrusted(true) // the policy reads the .git just made
	if err := h.SetPermMode("auto"); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\ntouch " + filepath.Join(outsideTemp(t), "hooked") + "\n"
	for _, c := range []struct {
		name, cmd, hook string
		ok              bool
		hint, asked     string // how the Sandbox line and the ask's reason start, "" for none
	}{
		{"commit on a new branch", "git checkout -qb x && echo a > a && git add a && git commit -qm a && git stash list", "", true, "", ""},
		{"branches, tags, stash, reset, merge and revert", "git branch y && git tag v1 && git tag -d v1 && echo b > a && git stash && git stash pop && git reset -q --hard && git checkout -q -- . && git merge -q x && git switch -qc r && git revert --no-edit HEAD && git switch -q x", "", true, "", ""},
		{"a branch from a remote one claims no upstream", "out=$(git checkout -b t o/base 2>&1) && case $out in *track*) exit 1 ;; esac && git checkout -q x", "", true, "", ""},
		{"a branch delete asks", "git branch -D y", "", false, "", "git branch writes what the sandbox keeps read-only"},
		{"gc asks", "git gc -q", "", false, "", "git gc can drop unreachable commits"},
		{"rebase asks", "git rebase -q HEAD~1", "", false, "", "git rebase writes what the sandbox keeps read-only"},
		{"config asks", "git config core.hooksPath /tmp/x", "", false, "", "git config writes what the sandbox keeps read-only"},
		{"push asks", "git push", "", false, "", "git push reaches the network"},
		{"config through a script", "printf 'git config core.hooksPath /tmp/x\\n' > $TMPDIR/c.sh && sh $TMPDIR/c.sh", "", false, ".git/config is read-only to commands: git would run what it names; call it again with sandbox: false", ""},
		{"a hook write", "echo x > .git/hooks/pre-commit", "", false, ".git/hooks/pre-commit is read-only to commands", ""},
		{"a hook that writes outside", "git commit -q --allow-empty -m b", hook, false, "commands write only in", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.hook != "" {
				if err := os.WriteFile(filepath.Join(proj, ".git", "hooks", "pre-commit"), []byte(c.hook), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			ui.asked = nil
			resp := run(map[string]any{"command": c.cmd})
			if resp.IsError == c.ok {
				t.Errorf("ok %v, want %v: %s", !resp.IsError, c.ok, resp.Content)
			}
			if _, hint, _ := strings.Cut(resp.Content, "\nSandbox: "); !strings.HasPrefix(hint, c.hint) || (hint == "") != (c.hint == "") {
				t.Errorf("hint %q, want %q: %s", hint, c.hint, resp.Content)
			}
			if asked := len(ui.asked) > 0 && strings.HasPrefix(ui.asked[0].Reason, c.asked); asked != (c.asked != "") || len(ui.asked) > 0 && c.asked == "" {
				t.Errorf("asked %+v, want reason %q", ui.asked, c.asked)
			}
		})
	}
	if out, err := h.git(proj, "log", "-1", "--format=%s", "x"); err != nil || strings.TrimSpace(out) != "a" {
		t.Errorf("branch x: %v %s", err, out)
	}
}

// TestSandboxedBuilds: go test and cargo build pass in the sandbox without an
// ask, side by side on one host.
func TestSandboxedBuilds(t *testing.T) {
	t.Parallel()
	h, _, ui, run := sandboxedShell(t)
	if err := h.SetPermMode("auto"); err != nil {
		t.Fatal(err)
	}
	t.Run("go test", func(t *testing.T) {
		t.Parallel()
		os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "go.mod"), []byte("module m\n\ngo 1.22\n"), 0o644)
		os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "m_test.go"), []byte(goTempTest), 0o644)
		if resp := run(map[string]any{"command": "cd " + h.Dirs.ProjectDir + " && go test -count=1 ./...", "timeout_ms": 300000.0}); resp.IsError || !strings.Contains(resp.Content, "ok") || len(ui.asked) > 0 {
			t.Errorf("go test in the sandbox: %s, asked %+v", resp.Content, ui.asked)
		}
	})
	t.Run("cargo build", func(t *testing.T) {
		t.Parallel()
		if _, err := exec.LookPath("cargo"); err != nil {
			t.Skip("no cargo")
		}
		crate := filepath.Join(h.Dirs.ProjectDir, "crate")
		os.MkdirAll(filepath.Join(crate, "src"), 0o755)
		os.WriteFile(filepath.Join(crate, "Cargo.toml"), []byte("[package]\nname = \"c\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"), 0o644)
		os.WriteFile(filepath.Join(crate, "src", "main.rs"), []byte("fn main() {}\n"), 0o644)
		resp := run(map[string]any{"command": "cd " + crate + " && cargo build --offline", "timeout_ms": 300000.0})
		if _, err := os.Stat(filepath.Join(crate, "target", "debug", "c")); err != nil || resp.IsError || len(ui.asked) > 0 {
			t.Errorf("cargo build in the sandbox: %v: %s, asked %+v", err, resp.Content, ui.asked)
		}
	})
}

// goTempTest resolves its temp dir, which sits under the hidden state dir, as
// cargo and realpath do with theirs.
const goTempTest = `package m

import (
	"path/filepath"
	"testing"
)

func TestTemp(t *testing.T) {
	if _, err := filepath.EvalSymlinks(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
`

// TestSandboxPolicy: the project is writable only when trusted, outside
// a read-only agent, with a backend that keeps .git and .agents read-only inside it,
// when it neither holds nor sits in one of zemi's dirs, holds no hidden path
// on a backend that cannot hide inside a write root, and its .git is not a
// symlink; zemi's dirs are hidden unless they hold the project, builds
// get a Go cache of their own and $TMPDIR is the scratchpad.
func TestSandboxPolicy(t *testing.T) {
	h, _ := newTestHost(t)
	h.Dirs.StateHome = t.TempDir()
	h.Dirs.ConfigHome = t.TempDir()
	h.Dirs.DataHome = t.TempDir()
	h.agents.sessionID = "s1"
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
			p := h.sandboxPolicy(c.plan, h.ScratchDir())
			if got := slices.Contains(p.Write, h.Dirs.ProjectDir); got != c.want {
				t.Errorf("project writable %v, want %v: %v", got, c.want, p.Write)
			}
			if !slices.Contains(p.Hide, h.Dirs.StateHome) || !slices.Contains(p.Hide, h.Dirs.DataHome) || slices.Contains(p.Hide, h.Dirs.ConfigHome) == c.projectInConfig {
				t.Errorf("hide %v", p.Hide)
			}
			// what the embedded runtime rewrites every start reads back
			if !slices.Contains(p.Read, h.TypesDir()) || !slices.Contains(p.Read, h.builtinSkillsDir()) {
				t.Errorf("read %v", p.Read)
			}
			// HOME moved: the account's own credentials stay hidden too
			if u, err := user.Current(); c.secretInside != "" && (err != nil || !slices.Contains(p.Hide, filepath.Join(u.HomeDir, ".ssh"))) {
				t.Errorf("account home's .ssh not hidden (%v): %v", err, p.Hide)
			}
			for _, dir := range []string{p.Env["GOCACHE"], p.Env["TMPDIR"]} {
				if st, err := os.Stat(dir); err != nil || !st.IsDir() || !slices.Contains(p.Write, dir) || !strings.HasPrefix(dir, h.Dirs.StateHome) {
					t.Errorf("GOCACHE or TMPDIR %q: %v, write %v", dir, err, p.Write)
				}
			}
			if p.Env["TMPDIR"] != h.ScratchDir() {
				t.Errorf("TMPDIR %q, want the scratchpad %q", p.Env["TMPDIR"], h.ScratchDir())
			}
		})
	}
	// seatbelt opens a .git dir but for what runs code; a .git file's git dir
	// (a linked worktree) stays shut
	h.SetProjectTrusted(true)
	h.sandbox.Store(&sandboxProbe{backend: sandbox.Backend{Name: "seatbelt", EnforcesProtect: true}})
	gd := filepath.Join(project, ".git")
	for _, dir := range []bool{true, false} {
		if dir {
			os.Mkdir(gd, 0o755)
		} else if err := os.RemoveAll(gd); err != nil || os.WriteFile(gd, []byte("gitdir: "+t.TempDir()), 0o644) != nil {
			t.Fatal(err)
		}
		if p := h.sandboxPolicy(false, h.ScratchDir()); (p.Git == gd) != dir || !slices.Contains(p.Protect, gd) {
			t.Errorf(".git a dir %v: git %v, protect %v", dir, p.Git, p.Protect)
		}
	}
	os.Remove(gd)
	// the machine's temp dirs are write roots; a backend that cannot hide
	// inside one (Landlock adds access and never denies) keeps out the root
	// holding zemi's own dirs
	for _, backend := range []string{"seatbelt", "fake"} {
		h.sandbox.Store(&sandboxProbe{backend: sandbox.Backend{Name: backend, EnforcesProtect: true}})
		temp := perm.TempRoots()[0]
		if !h.inJail(h.Dirs.StateHome, temp) {
			t.Fatalf("the state dir %s is not under %s", h.Dirs.StateHome, temp)
		}
		if got := slices.Contains(h.sandboxPolicy(false, h.ScratchDir()).Write, temp); got != (backend == "seatbelt") {
			t.Errorf("%s: %s writable %v", backend, temp, got)
		}
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
		p := h.sandboxPolicy(false, h.ScratchDir())
		if got := slices.Contains(p.Write, project); got != c.writable {
			t.Errorf("%s: project writable %v: %v", c.name, got, p.Write)
		}
		if got := slices.Contains(p.Protect, venv); got != c.protect {
			t.Errorf("%s: %s protected %v: %v", c.name, venv, got, p.Protect)
		}
	}
	h.Dirs.CacheHome = t.TempDir()
	server := h.serverPolicy(lspDef{Name: "rust-analyzer"})
	dir := filepath.Join(h.Dirs.CacheHome, "lsp", trustKey(h.trustRoot()), "rust-analyzer")
	if !slices.Equal(server.Write, []string{dir}) || !server.Loopback || !slices.Contains(server.Hide, h.Dirs.StateHome) {
		t.Errorf("server policy %+v", server)
	}
	for _, k := range []string{"CARGO_TARGET_DIR", "GOCACHE", "XDG_CACHE_HOME", "TMPDIR"} {
		if st, err := os.Stat(server.Env[k]); err != nil || !st.IsDir() || !strings.HasPrefix(server.Env[k], dir) {
			t.Errorf("server %s %q: %v", k, server.Env[k], err)
		}
	}
	lls := h.serverPolicy(lspDef{Name: "lua-language-server", Root: h.Dirs.ConfigHome})
	if !slices.Contains(lls.Read, h.TypesDir()) || !slices.Contains(lls.Read, h.Dirs.ConfigHome) || !slices.Contains(lls.Hide, filepath.Join(h.Dirs.ConfigHome, ".git")) {
		t.Errorf("config tree server policy %+v", lls)
	}
	a := h.sandboxPolicy(false, h.ScratchDir()).Env["GOCACHE"]
	h.Dirs.ProjectDir = t.TempDir()
	h.trustRoot = sync.OnceValue(h.Dirs.TrustRoot) // a host finds its root once
	if b := h.sandboxPolicy(false, h.ScratchDir()).Env["GOCACHE"]; a == b {
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
		done := h.guardSandbox(h.sandboxPolicy(false, h.ScratchDir()))
		// what `mkdir .agents && echo 'print(1)' > .agents/init.lua` leaves behind
		agentsDir := filepath.Join(h.Dirs.ProjectDir, ".agents")
		if err := os.MkdirAll(agentsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(agentsDir, "init.lua"), []byte("print(1)"), 0o644); err != nil {
			t.Fatal(err)
		}
		if running {
			h.CheckSandboxed()
			if h.ProjectTrusted() {
				t.Error("running: still trusted")
			}
		}
		note := done()
		if h.ProjectTrusted() || !h.TrustHeld() || !strings.Contains(note, agentsDir) || !strings.Contains(note, "/trust") {
			t.Errorf("running %v: trusted %v held %v note %q", running, h.ProjectTrusted(), h.TrustHeld(), note)
		}
	}
	h, _ := newTestHost(t)
	h.SetProjectTrusted(true)
	h.sandbox.Store(&sandboxProbe{backend: sandbox.Backend{Name: "fake", EnforcesProtect: true}})
	if note := h.guardSandbox(h.sandboxPolicy(false, h.ScratchDir()))(); note != "" || !h.ProjectTrusted() {
		t.Errorf("nothing created: note %q trusted %v", note, h.ProjectTrusted())
	}
}

// TestSandboxNestedConfig: on a backend that protects only the paths it
// binds, a .git or .agents a sandboxed command creates anywhere in the project
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
		{"nested .agents", "", "a/b/.agents/init.lua", 0, filepath.Join("b", ".agents")},
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
			p := h.sandboxPolicy(false, h.ScratchDir())
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
	if p := h.sandboxPolicy(false, h.ScratchDir()); slices.Contains(p.Write, h.Dirs.ProjectDir) {
		t.Errorf("a project too big to walk is writable: %v", p.Write)
	}
}

func TestTrustUnsaved(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	// roots are resolved, so a HOME spelled through a link must still match
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", link)
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

func TestWorktreesDir(t *testing.T) {
	h, _ := newTestHost(t)
	h.Dirs.StateHome = t.TempDir()
	h.SetProjectTrusted(true)
	h.sandbox.Store(&sandboxProbe{backend: sandbox.Backend{Name: "fake", EnforcesProtect: true}})
	project := h.Dirs.ProjectDir
	agents := filepath.Join(project, ".agents")
	wt := filepath.Join(agents, "worktrees", "x")
	for _, d := range []string{filepath.Join(wt, ".agents"), filepath.Join(agents, "skills", ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if p := h.sandboxPolicy(false, h.ScratchDir()); !slices.Contains(p.Write, filepath.Dir(wt)) || !slices.Contains(p.Protect, filepath.Join(wt, ".agents")) {
			t.Fatalf("write %v protect %v", p.Write, p.Protect)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(agents, "worktrees", ".gitignore")); string(b) != "/*\n" {
		t.Fatalf(".gitignore %q", b)
	}
	if found, _ := configEntries(project); found[filepath.Join(agents, "skills", ".git")] {
		t.Fatalf("walked .agents/skills: %v", found)
	}
	for path, want := range map[string]bool{
		filepath.Join(wt, "main.go"):             false,
		filepath.Join(wt, ".agents", "init.lua"): true,
		filepath.Join(wt, ".git"):                true,
		filepath.Join(agents, "init.lua"):        true,
		filepath.Join(agents, "lua", "m.lua"):    true,
		filepath.Join(project, "pkg", "a.go"):    false,
		filepath.Join(wt, "pkg", ".git", "cfg"):  true,
	} {
		if got := h.protectedWrite(path); got != want {
			t.Errorf("protectedWrite(%s) = %v, want %v", path, got, want)
		}
	}
}

// TestSandboxPrompt: every clause of the Sandbox block follows the policy
// and the escapes the run has; without them no UI can answer an ask either.
// A run with no scratchpad has no $TMPDIR, so no clause may promise one.
func TestSandboxPrompt(t *testing.T) {
	for _, c := range []struct {
		name                            string
		trusted, live, readOnly, escape bool
		gitDir                          bool
		noPad                           bool
		want, unwanted                  []string
	}{
		{"trusted", true, true, false, true, false, false, []string{
			"They write only the project, the temp dirs, $TMPDIR and $GOCACHE.\n",
			".git and .agents are read-only to them: write .agents files with the write tool",
			"Hidden from them: zemi's own dirs (", "Read zemi's files with the read tool.",
			"secret-shaped files (.env, *.pem, *.key), in $TMPDIR too",
			"Network: localhost only. A unix socket in $TMPDIR binds and dials; one outside it, the ssh agent's or docker's, does not. network: true adds the network, sandbox: false drops every limit",
			"a browser, GUI app, docker or the keychain fails or crashes (Abort trap: 6). Run one with sandbox: false.",
		}, []string{"read-only to this agent"}},
		{"trusted with a .git dir", true, true, false, true, true, false, []string{
			".agents and .git's config and hooks are read-only to them",
			"git commit, checkout and stash run confined; git config, remote, rebase, push and fetch ask the user first.",
		}, []string{"change .git"}},
		{"read-only agent", true, true, true, true, false, false, []string{
			"They write only the temp dirs, $TMPDIR and $GOCACHE: the project is read-only to this agent.\n",
			"network: true adds the network (needs approval).", "Do not run one: say what you could not check.",
		}, []string{"sandbox: false", ".git and .agents"}},
		{"nobody to approve", true, true, false, false, false, false, []string{"Nobody can approve more in this run.", "Do not run one", "change .git need the user's approval, which nobody can give"}, []string{"sandbox: false", "network: true", "ask first"}},
		{"untrusted", false, true, false, true, false, false, []string{"this project is not trusted", "ask the user first, and run unconfined"}, []string{"sandbox: false"}},
		{"untrusted with nobody to approve", false, true, false, false, false, false, []string{"other commands need the user's approval, which nobody can give"}, []string{"ask the user", "unconfined"}},
		{"no backend", true, false, false, true, false, false, []string{"none on this machine", "ask the user first"}, []string{"sandbox: false"}},
		{"no backend with nobody to approve", true, false, false, false, false, false, []string{"which nobody can give in this run"}, []string{"ask the user first"}},
		{
			name: "no pad", trusted: true, live: true, noPad: true,
			unwanted: []string{"A unix socket in $TMPDIR", "Other tools' caches"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestHost(t)
			h.Dirs.ConfigHome, h.Dirs.DataHome, h.Dirs.StateHome = t.TempDir(), t.TempDir(), t.TempDir()
			h.agents.sessionID = "s1"
			s := &sandboxProbe{backend: sandbox.Backend{Name: "seatbelt", HidesSecretFiles: true, EnforcesProtect: true}}
			if !c.live {
				s.err = errors.New("no backend")
			}
			h.sandbox.Store(s)
			if c.escape {
				h.SetUIHooks(&fakeUI{})
			}
			if c.gitDir {
				os.Mkdir(filepath.Join(h.Dirs.ProjectDir, ".git"), 0o755)
			}
			h.SetProjectTrusted(c.trusted)
			pad := h.ScratchDir()
			if c.noPad {
				pad = ""
			}
			got := h.sandboxPrompt(tools.All(&tools.Pipeline{SandboxParam: c.escape}), c.readOnly, pad)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("lacks %q:\n%s", w, got)
				}
			}
			for _, w := range c.unwanted {
				if strings.Contains(got, w) {
					t.Errorf("says %q:\n%s", w, got)
				}
			}
		})
	}
}
