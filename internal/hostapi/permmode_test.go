package hostapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// TestAutoModeLocalApprovals: auto mode settles reads and writes inside the
// jail on its own. A path outside it, a secret behind an innocent name, and
// anything leaving the machine go to the user instead.
func TestAutoModeLocalApprovals(t *testing.T) {
	h, _ := newTestHost(t)
	fakeSandbox(h, true)
	h.Dirs.ConfigHome = filepath.Join(h.Dirs.ProjectDir, ".config", "zemi") // zemi run in $HOME
	h.Dirs.StateHome = t.TempDir()
	inside := filepath.Join(h.Dirs.ProjectDir, "main.go")
	secret := filepath.Join(h.Dirs.ProjectDir, ".env")
	link := filepath.Join(h.Dirs.ProjectDir, "notes.md")
	os.WriteFile(secret, []byte("K=v"), 0o600)
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	// the kernel resolves out/.. from out's target, outside the project
	outside := t.TempDir()
	os.Mkdir(filepath.Join(outside, "sub"), 0o700)
	if err := os.Symlink(filepath.Join(outside, "sub"), filepath.Join(h.Dirs.ProjectDir, "out")); err != nil {
		t.Fatal(err)
	}
	// git init --separate-git-dir
	os.MkdirAll(filepath.Join(h.Dirs.ProjectDir, "gitstore", "hooks"), 0o755)
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, ".git"), []byte("gitdir: gitstore\n"), 0o644)
	for _, c := range []struct {
		subject, arg string
		want         bool
		untrusted    bool // what an untrusted project settles
	}{
		{"fs.write", filepath.Join(h.Dirs.ProjectDir, "gitstore", "hooks", "pre-commit"), false, false},
		{"fs.read", inside, true, true},
		{"fs.write", inside, true, false},
		{"tool.bash", "go test ./...", true, false},
		{"tool.bash", "git status", true, false},
		{"tool.bash", "ls && sed -n 1,5p main.go", true, true},
		{"fs.write", filepath.Join(h.Dirs.ProjectDir, ".git", "config"), false, false},
		{"fs.write", filepath.Join(h.Dirs.ProjectDir, "sub", ".git", "hooks", "pre-commit"), false, false},
		{"fs.write", filepath.Join(h.Dirs.ConfigHome, "init.lua"), false, false},
		{"fs.write", filepath.Join(h.Dirs.ProjectDir, ".zemi", "init.lua"), false, false},
		{"fs.write", filepath.Join(h.Dirs.ProjectDir, "pkg", ".zemi", "init.lua"), false, false},
		{"fs.read", secret, false, false},
		{"fs.read", link, false, false},
		{"fs.read", "/etc/passwd", false, false},
		{"fs.read", filepath.Join(h.Dirs.LogDir(), "run.log"), false, false}, // server stderr may hold tokens
		{"fs.read", h.Dirs.ProjectDir + "/out/../loot.txt", false, false},
		{"fs.write", h.Dirs.ProjectDir + "/out/../victim.txt", false, false},
		{"tool.bash", "cat out/../loot.txt", true, false}, // the sandbox holds it
		{"tool.bash", "rm -rf out", false, false},
		{"tool.bash", "cat main.go", true, true},
		{"fs.read", filepath.Join(os.Getenv("HOME"), ".ssh", "id_ed25519"), false, false},
		{"net.evil.tld", "https://evil.tld/?d=leak", false, false},
	} {
		for _, trusted := range []bool{true, false} {
			h.SetProjectTrusted(trusted)
			want := map[bool]bool{true: c.want, false: c.untrusted}[trusted]
			if got := h.autoLocal(tools.Request{Subject: c.subject, Arg: c.arg}); got != want {
				t.Errorf("trusted=%v %s %s: got %v, want %v", trusted, c.subject, c.arg, got, want)
			}
		}
	}
}

// TestUntrustedModes: in an untrusted project, for a command with no
// sandbox to run in, or for a write outside the project, auto mode sends
// what it cannot settle to the user; the Lua tool asks there and for a
// read-only agent.
func TestUntrustedModes(t *testing.T) {
	h, _ := newTestHost(t)
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "main.go"), []byte("package main"), 0o600)
	ask := h.askHook() // no UI: whatever reaches the user is denied
	for _, c := range []struct {
		mode          string
		trusted, live bool
		subject       string
		arg           string
		want          bool
	}{
		// no server annotated it read-only, so no rule settles it
		{"auto", true, true, "mcp.gh", "run", false},
		{"auto", false, true, "mcp.gh", "run", false},
		{"auto", false, true, "tool.bash", "cat main.go", true},
		{"auto", true, true, "tool.bash", "make out", true},
		{"auto", true, false, "tool.bash", "make out", false},
		{"auto", true, true, "fs.write", filepath.Join(os.Getenv("HOME"), ".gitconfig"), false},
	} {
		if err := h.SetPermMode(c.mode); err != nil {
			t.Fatal(err)
		}
		h.SetProjectTrusted(c.trusted)
		fakeSandbox(h, c.live)
		ok, _, why := ask(tools.Request{Subject: c.subject, Arg: c.arg})
		if ok != c.want {
			t.Errorf("%s trusted=%v live=%v %s %q: got %v", c.mode, c.trusted, c.live, c.subject, c.arg, ok)
		}
		if untrusted := "this project is not trusted (/trust)"; c.mode == "auto" && !c.trusted && !ok && why != untrusted {
			t.Errorf("%s %q: ask reason %q", c.subject, c.arg, why)
		}
	}
	// the reason names what kept the mode from settling it
	h.SetProjectTrusted(true)
	fakeSandbox(h, true)
	h.SetPermMode("auto")
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, ".env"), []byte("K=1"), 0o600)
	for _, c := range []struct{ subject, arg, why string }{
		{"fs.read", filepath.Join(h.Dirs.ProjectDir, ".env"), "this file looks like it holds secrets"},
		{"net.example.com", "https://example.com/", "a fetch always asks: the model chose the address"},
	} {
		if ok, _, why := ask(tools.Request{Subject: c.subject, Arg: c.arg}); ok || why != c.why {
			t.Errorf("%s %s: allowed %v, reason %q", c.subject, c.arg, ok, why)
		}
	}
	// a mode's own rule does not settle a protected write any store rule re-asks
	runUser(t, h.RT, `zemi.permission.mode.add("open", { base = "auto", rules = { { "fs.write", "*", "allow" } } })`)
	h.SetPermMode("open")
	for _, f := range []string{".git/config", ".gitattributes"} {
		if ok, _, _ := ask(tools.Request{Subject: "fs.write", Arg: filepath.Join(h.Dirs.ProjectDir, f)}); ok {
			t.Errorf("mode rule settled a write to %s", f)
		}
	}
	h.SetPermMode("auto")
	lua := h.luaEvalTool()
	for _, c := range []struct {
		mode     string
		trusted  bool
		readOnly bool
		ran      bool
	}{
		{"auto", true, false, true},
		{"auto", false, false, false},
		{"auto", true, true, false},
	} {
		h.SetPermMode(c.mode)
		if c.readOnly {
			h.SetProfile("planner")
		} else {
			h.SetProfile("default")
		}
		h.SetProjectTrusted(c.trusted)
		resp, err := lua.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "lua", Input: `{"description":"x","script":"return 1"}`})
		if err != nil || strings.Contains(resp.Content, "return: 1") != c.ran {
			t.Errorf("Lua %s trusted=%v readOnly=%v: %q %v", c.mode, c.trusted, c.readOnly, resp.Content, err)
		}
	}
}

// TestAutoModeReadCommands: with no sandbox only readers settle, never
// from the scratchpad's directory, and a recursive search only over trees
// without secret-shaped files.
func TestAutoModeReadCommands(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	fakeSandbox(h, false)
	h.SetProjectTrusted(true)
	h.Dirs.ConfigHome = t.TempDir()
	h.agents.sessionID = "s1"
	pad, root := h.ScratchDir(), h.Dirs.ProjectDir
	for _, d := range []string{pad, filepath.Join(root, "internal"), filepath.Join(root, "sub")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, ".env"), []byte("K=v"), 0o600)
	os.WriteFile(filepath.Join(root, "internal", "x.go"), []byte("package x"), 0o600)
	os.WriteFile(filepath.Join(pad, "out.txt"), []byte("hi"), 0o600)
	os.WriteFile(filepath.Join(h.Dirs.ConfigHome, "init.lua"), []byte("-- cfg"), 0o600)
	if err := os.Symlink(filepath.Join(root, ".env"), filepath.Join(root, "sub", "notes")); err != nil {
		t.Fatal(err)
	}
	// rg skips hidden and gitignored entries below its argument, grep -r does not
	os.MkdirAll(filepath.Join(root, "app", "node_modules"), 0o700)
	os.WriteFile(filepath.Join(root, "app", ".env"), []byte("K=v"), 0o600)
	os.WriteFile(filepath.Join(root, "app", ".gitignore"), []byte("node_modules/\n"), 0o600)
	os.WriteFile(filepath.Join(root, "app", "node_modules", "secret.txt"), []byte("K=v"), 0o600)
	// an ignored tree past the walk limit does not count against rg
	for i := range 10001 {
		os.WriteFile(filepath.Join(root, "app", "node_modules", fmt.Sprint(i)), nil, 0o600)
	}
	os.MkdirAll(filepath.Join(root, "wl"), 0o700)
	os.WriteFile(filepath.Join(root, "wl", ".env"), []byte("K=v"), 0o600)
	os.WriteFile(filepath.Join(root, "wl", ".rgignore"), []byte("!.env\n"), 0o600)
	// git on macOS ignores Build/ for build/, rg does not
	os.MkdirAll(filepath.Join(root, "ic", "Build"), 0o700)
	os.WriteFile(filepath.Join(root, "ic", ".gitignore"), []byte("build/\n"), 0o600)
	os.WriteFile(filepath.Join(root, "ic", "Build", "secret.txt"), []byte("K=v"), 0o600)
	if _, err := h.git(root, "init", "-q"); err != nil {
		t.Skip("needs git:", err)
	}
	h.git(root, "config", "core.ignorecase", "true")
	for _, c := range []struct {
		cmd  string
		want bool
	}{
		{"cat " + pad + "/out.txt", true},
		{"grep -n hi " + pad + "/out.txt", true},
		// zemi's own config reads like the project; running code from it does not
		{"cat " + filepath.Join(h.Dirs.ConfigHome, "init.lua"), true},
		{"grep -rn zemi " + h.Dirs.ConfigHome, true},
		{"cd " + pad + " && cat out.txt", false},
		{"grep -rn foo internal/", true},
		{"rg -n foo internal", true},
		{"diff -r internal internal", true},
		{"grep -r foo .", false},
		{"grep -r foo", false},
		{"rg KEY", false},
		{"rg -t go KEY", false},
		{"ag KEY", false},
		{"diff -r . internal", false},
		{"grep -rn foo sub", false},
		{"cd sub && cat notes", false},
		{"cd internal && cat x.go", true},
		{"cat .git/config", false},
		{"rg KEY app", true},
		{"rg KEY wl", false},
		{"rg KEY ic", false},
		{"rg -eKEY wl internal", false},
		{"rg --regexp=KEY wl internal", false},
		{"rg -e KEY internal", true},
		{"grep -rn foo internal/ --exclude-dir=.git", true},
		{"rg -t go foo internal", true},
		{"grep -rn -A 3 foo internal/", true},
		{"rg -g '*.go' foo internal", true},
		{"grep -r -eKEY app internal", false},
		{"rg --hidden KEY app", false},
		{"rg -nuu KEY app", false},
		{"rg --no-ignore-vcs KEY app", false},
		{"rg -g x KEY app", false},
		{"grep -r KEY app", false},
		{"ag KEY app", false},
	} {
		if got := h.autoLocal(tools.Request{Subject: "tool.bash", Arg: c.cmd}); got != c.want {
			t.Errorf("%s: got %v, want %v", c.cmd, got, c.want)
		}
	}
}

// TestAutoResolvesPaths: auto mode judges a file request where it lands. A
// project symlink into zemi's config, into the repository's git dir or out
// of the project goes to the user, and the overlay names the real path.
func TestAutoResolvesPaths(t *testing.T) {
	h, _ := newTestHost(t)
	h.Dirs.ConfigHome = t.TempDir()
	h.SetProjectTrusted(true)
	if err := h.SetPermMode("auto"); err != nil {
		t.Fatal(err)
	}
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	proj, outside := h.Dirs.ProjectDir, perm.RealPath(t.TempDir())
	os.MkdirAll(filepath.Join(proj, ".git", "hooks"), 0o755)
	os.Symlink(h.Dirs.ConfigHome, filepath.Join(proj, "cfg"))
	os.Symlink(filepath.Join(proj, ".git"), filepath.Join(proj, "gl"))
	os.Symlink(outside, filepath.Join(proj, "out"))
	ask := h.askHook()
	for _, c := range []struct{ arg, shown string }{
		{"cfg/init.lua", filepath.Join(perm.RealPath(h.Dirs.ConfigHome), "init.lua")},
		{"gl/hooks/post-checkout", filepath.Join(".git", "hooks", "post-checkout")},
		{"out/x.go", filepath.Join(outside, "x.go")},
	} {
		ui.asked = nil
		ask(tools.Request{Subject: "fs.write", Arg: filepath.Join(proj, c.arg)})
		if len(ui.asked) != 1 || ui.asked[0].Arg != c.shown {
			t.Errorf("%s: asked %+v", c.arg, ui.asked)
		}
	}
}

// TestPermModeDecide: a mode answers asks from Lua, off the interpreter and
// without the veto deadline, so a decide that calls a model is possible.
// What it does not decide goes to the user even where the base would settle
// it (no UI here: denied), which is how a mode asks about everything.
func TestPermModeDecide(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `zemi.permission.mode.add("review", { base = "auto", decide = function(req)
		if req.arg == "ls" then return "allow" end
		if req.arg == "rm -rf /" then return "deny" end
	end })`)
	if err := h.SetPermMode("review"); err != nil {
		t.Fatal(err)
	}
	ask := h.askHook()
	for cmd, want := range map[string]bool{"ls": true, "rm -rf /": false, "cat x": false} {
		if ok, by, _ := ask(tools.Request{Subject: "tool.bash", Arg: cmd}); ok != want {
			t.Errorf("%q: allowed %v by %s", cmd, ok, by)
		}
	}
}

// TestPermissionsOverlayRemovesAlways: a mis-keyed "always" answer is the
// only rule a user writes at runtime and the only one with no file to edit,
// so /permissions shows it and takes it back.
func TestPermissionsOverlayRemovesAlways(t *testing.T) {
	h, _ := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	always := perm.Rule{Subject: "tool.bash", Pattern: "git push *", Decision: perm.Allow, Principal: "model"}
	h.Perm.Add(always)
	h.Perm.Add(perm.Rule{Subject: "tool.bash", Pattern: "rm -rf *", Decision: perm.Deny})
	h.Perm.Add(perm.Rule{Subject: "tool.bash", Pattern: "make *", Decision: perm.Allow, Source: "permissions"})
	h.Perm.Add(perm.Rule{Subject: "tool.bash", Pattern: "curl *", Decision: perm.Ask, Source: "project"})
	if err := h.OpenPermissions(); err != nil || len(ui.opened) != 1 {
		t.Fatalf("open: %v %+v", err, ui.opened)
	}
	rows := fmt.Sprint(ui.opened[0].Items)
	if !strings.Contains(rows, "tool.bash git push *") || !strings.Contains(rows, "run:permissions forget tool.bash git push *") {
		t.Fatalf("rules not listed with a way back:\n%s", rows)
	}
	// each rule says where to edit it
	for _, want := range []string{"from .zemi/permissions; edit that file", "from the project's .zemi/init.lua", "from config; edit permissions in your init.lua"} {
		if !strings.Contains(rows, want) {
			t.Fatalf("missing %q:\n%s", want, rows)
		}
	}
	if err := h.ForgetRule("tool.bash rm -rf *"); err == nil {
		t.Error("a config rule must not be removable: a reload brings it back")
	}
	if err := h.ForgetRule("tool.bash git push *"); err != nil {
		t.Fatal(err)
	}
	if h.Perm.Evaluate("tool.bash", "git push origin main", "model").Decision != perm.Ask {
		t.Error("the removed rule still allows")
	}
}

// TestExplicitAskReachesUser: an ask rule and a not_match exclusion are the
// user's under auto, where the mode would settle the same request.
func TestExplicitAskReachesUser(t *testing.T) {
	h, _ := newTestHost(t)
	fakeSandbox(h, true)
	h.SetProjectTrusted(true)
	if err := h.SetPermMode("auto"); err != nil {
		t.Fatal(err)
	}
	proj := h.Dirs.ProjectDir
	os.MkdirAll(filepath.Join(proj, "vendor"), 0o755)
	os.WriteFile(filepath.Join(proj, "vendor", "x.go"), []byte("package x"), 0o644)
	h.Perm.Add(perm.Rule{Subject: "tool.bash", Pattern: "go test*", Decision: perm.Ask})
	h.Perm.Add(perm.Rule{Subject: "fs.read", Pattern: filepath.Join(proj, "*"), Decision: perm.Allow, NotMatch: filepath.Join(proj, "vendor", "*")})
	p := h.PipelineFor()
	for _, c := range []struct {
		subject, arg string
		settled      bool
	}{
		{"tool.bash", "go test ./...", false},
		{"tool.bash", "go vet ./...", true},
		{"fs.read", filepath.Join(proj, "vendor", "x.go"), false},
	} {
		v := p.Check(tools.Request{Subject: c.subject, Arg: c.arg})
		// no UI: an ask the mode leaves to the user is denied by default
		if got := v.Decision == perm.Allow; got != c.settled || !c.settled && !strings.Contains(v.Reason, "a rule asks for this") {
			t.Errorf("%s %q: %+v", c.subject, c.arg, v)
		}
	}
}
