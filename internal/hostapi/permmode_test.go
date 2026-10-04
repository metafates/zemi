package hostapi

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
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
	h.Dirs.StateHome, h.Dirs.DataHome = t.TempDir(), t.TempDir()
	plugin := filepath.Join(h.Dirs.PluginsRoot(), "local", "p", "init.lua")
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
	// a symlinked .agents: its target runs as project config too
	os.Mkdir(filepath.Join(h.Dirs.ProjectDir, "cfg"), 0o700)
	if err := os.Symlink("cfg", filepath.Join(h.Dirs.ProjectDir, ".agents")); err != nil {
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
		{"tool.shell", "go test ./...", true, false},
		{"tool.shell", "git status", true, false},
		{"tool.shell", "ls && sed -n 1,5p main.go", true, true},
		{"fs.write", filepath.Join(h.Dirs.ProjectDir, ".git", "config"), false, false},
		{"fs.write", filepath.Join(h.Dirs.ProjectDir, "sub", ".git", "hooks", "pre-commit"), false, false},
		{"fs.write", filepath.Join(h.Dirs.ConfigHome, "init.lua"), false, false},
		{"fs.write", filepath.Join(h.Dirs.ProjectDir, ".agents", "init.lua"), false, false},
		{"fs.write", filepath.Join(h.Dirs.ProjectDir, "pkg", ".agents", "init.lua"), false, false},
		{"fs.write", filepath.Join(h.Dirs.ProjectDir, ".agents", "skills", "x", "SKILL.md"), true, false}, // runs nothing
		{"fs.write", filepath.Join(h.Dirs.ProjectDir, "cfg", "init.lua"), false, false},
		{"fs.read", secret, false, false},
		{"fs.read", link, false, false},
		{"fs.read", "/etc/passwd", false, false},
		{"fs.read", filepath.Join(h.Dirs.LogDir(), "run.log"), false, false}, // server stderr may hold tokens
		{"fs.read", h.Dirs.ProjectDir + "/out/../loot.txt", false, false},
		{"fs.write", h.Dirs.ProjectDir + "/out/../victim.txt", false, false},
		{"tool.shell", "cat out/../loot.txt", true, false}, // the sandbox holds it
		{"tool.shell", "rm -rf out", false, false},
		{"tool.shell", "cat main.go", true, true},
		{"fs.read", filepath.Join(os.Getenv("HOME"), ".ssh", "id_ed25519"), false, false},
		// a local plugin reads like the project, and a write loads it everywhere
		{"fs.read", plugin, true, true},
		{"tool.shell", "cat " + plugin, true, true},
		{"fs.write", plugin, false, false},
		{"fs.write", filepath.Join(h.Dirs.PluginsRoot(), "installed", "p", "init.lua"), false, false},
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

// TestPluginCreationGrant: the user's yes to the write that creates a
// local plugin lets auto edit that plugin, in an untrusted project too;
// its .git, a plugin that existed and one whose creation was refused keep
// asking, and zemi.fs never reaches it.
func TestPluginCreationGrant(t *testing.T) {
	h, _ := newTestHost(t)
	h.Dirs.DataHome = t.TempDir()
	local := filepath.Join(perm.RealPath(h.Dirs.DataHome), "plugins", "local")
	h.pluginsRoot = func() string { return filepath.Dir(local) }
	if err := os.MkdirAll(filepath.Join(local, "y"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.SetPermMode("auto")
	ui := &fakeUI{askBy: "user"}
	h.SetUIHooks(ui)
	ask := h.askHook()
	const creates = ", which zemi runs in every project; once allowed, zemi edits it without asking until it exits"
	for _, c := range []struct {
		path       string
		yes, asked bool
		allowed    bool
		why        string
	}{
		{"x/init.lua", true, true, true, "this creates the local plugin x" + creates},
		{"x/lua/x/init.lua", false, false, true, ""},
		{"x/.git/config", false, true, false, "this changes .git, which decides what git runs"},
		{"y/init.lua", true, true, true, "this changes a local plugin, which zemi runs in every project"},
		{"y/lua/y/init.lua", false, true, false, "this changes a local plugin, which zemi runs in every project"},
		{"z/init.lua", false, true, false, "this creates the local plugin z" + creates},
		{"z/lua/z/init.lua", false, true, false, "this creates the local plugin z" + creates},
	} {
		ui.asked, ui.askAllowed = nil, c.yes
		ok, _, why := ask(tools.Request{Subject: "fs.write", Arg: filepath.Join(local, c.path)})
		if ok != c.allowed || (len(ui.asked) > 0) != c.asked || why != c.why {
			t.Errorf("%s: allowed %v, asked %v, reason %q", c.path, ok, len(ui.asked) > 0, why)
		}
	}
	runExpectErr(t, h.RT, fmt.Sprintf("zemi.fs.write(%q, 'x')", filepath.Join(local, "x", "init.lua")), "outside the jail")
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
		{"auto", false, true, "tool.shell", "cat main.go", true},
		{"auto", true, true, "tool.shell", "make out", true},
		{"auto", true, false, "tool.shell", "make out", false},
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
		if untrusted := "this project is not trusted (zemi run --trust)"; c.mode == "auto" && !c.trusted && !ok && why != untrusted {
			t.Errorf("%s %q: ask reason %q", c.subject, c.arg, why)
		}
	}
	// the reason names what kept the mode from settling it
	h.SetProjectTrusted(true)
	fakeSandbox(h, true)
	h.SetPermMode("auto")
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, ".env"), []byte("K=1"), 0o600)
	for _, c := range []struct{ tool, subject, arg, why string }{
		{"read", "fs.read", filepath.Join(h.Dirs.ProjectDir, ".env"), "this file looks like it holds secrets"},
		{"fetch", "net.example.com", "https://example.com/", "a fetch always asks: the model chose the address"},
		{"web_search", "net.lite.duckduckgo.com", "https://lite.duckduckgo.com/lite/?q=x", "a search always asks: the model chose the query"},
	} {
		if ok, _, why := ask(tools.Request{Tool: c.tool, Subject: c.subject, Arg: c.arg}); ok || why != c.why {
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
	lua := h.luaEvalTool(h.PipelineFor())
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
		resp, err := lua.Run(t.Context(), fantasy.ToolCall{ID: "1", Name: "lua", Input: `{"about":"x","script":"return 1"}`})
		if err != nil || strings.Contains(resp.Content, "return: 1") != c.ran {
			t.Errorf("Lua %s trusted=%v readOnly=%v: %q %v", c.mode, c.trusted, c.readOnly, resp.Content, err)
		}
	}
}

// TestReadOnlyUnderYolo: yolo settles a read-only agent's changing command
// only when the sandbox confines it, and never an MCP tool not marked
// read-only.
func TestReadOnlyUnderYolo(t *testing.T) {
	h, _ := newTestHost(t)
	h.SetProjectTrusted(true)
	if err := h.SetProfile("planner"); err != nil {
		t.Fatal(err)
	}
	if err := h.SetPermMode("yolo"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		live         bool
		subject, arg string
		want         bool
	}{
		{true, "tool.shell", "sed -i s/a/b/ main.go", true},
		{false, "tool.shell", "sed -i s/a/b/ main.go", false},
		{false, "tool.shell", "ls", true},
		{true, "mcp.gitlab", "create_merge_request", false},
	} {
		fakeSandbox(h, c.live)
		if v := h.PipelineFor().Check(tools.Request{Subject: c.subject, Arg: c.arg}); (v.Decision == perm.Allow) != c.want {
			t.Errorf("live=%v %s %q: %+v", c.live, c.subject, c.arg, v)
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
	h.Dirs.ConfigHome, h.Dirs.DataHome = t.TempDir(), t.TempDir()
	h.agents.sessionID = "s1"
	pad, root := h.ScratchDir(), h.Dirs.ProjectDir
	for _, d := range []string{pad, filepath.Join(root, "internal"), filepath.Join(root, "sub"), h.TypesDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, ".env"), []byte("K=v"), 0o600)
	os.WriteFile(filepath.Join(root, "internal", "x.go"), []byte("package x"), 0o600)
	os.WriteFile(filepath.Join(pad, "out.txt"), []byte("hi"), 0o600)
	os.WriteFile(filepath.Join(h.Dirs.ConfigHome, "init.lua"), []byte("-- cfg"), 0o600)
	os.WriteFile(filepath.Join(h.TypesDir(), "zemi.lua"), []byte("---@meta"), 0o600)
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
		os.WriteFile(filepath.Join(root, "app", "node_modules", strconv.Itoa(i)), nil, 0o600)
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
	// a repository with no secret-shaped file: its .git/config may still hold a token
	os.MkdirAll(filepath.Join(root, "nest"), 0o700)
	os.WriteFile(filepath.Join(root, "nest", "a.txt"), nil, 0o600)
	h.git(filepath.Join(root, "nest"), "init", "-q")
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
		{"grep -n -B15 'function zemi.ui' " + filepath.Join(h.TypesDir(), "zemi.lua"), true},
		{"cd " + h.TypesDir() + " && cat zemi.lua", false},
		{"cat " + filepath.Join(h.Dirs.DataHome, "catwalk.json"), false},
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
		{"rg TOKEN nest", true},
		{"grep -rn TOKEN nest", false},
		{"rg --hidden TOKEN nest", false},
		{"diff -r nest nest", false},
	} {
		if got := h.autoLocal(tools.Request{Subject: "tool.shell", Arg: c.cmd, Pad: pad}); got != c.want {
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
	proj, outside := h.Dirs.ProjectDir, outsideTemp(t)
	os.MkdirAll(filepath.Join(proj, ".git", "hooks"), 0o755)
	os.Symlink(h.Dirs.ConfigHome, filepath.Join(proj, "cfg"))
	os.Symlink(filepath.Join(proj, ".git"), filepath.Join(proj, "gl"))
	os.Symlink(outside, filepath.Join(proj, "out"))
	pipe := h.PipelineFor()
	for _, c := range []struct{ arg, shown string }{
		{"cfg/init.lua", filepath.Join(perm.RealPath(h.Dirs.ConfigHome), "init.lua")},
		{"gl/hooks/post-checkout", filepath.Join(".git", "hooks", "post-checkout")},
		{"out/x.go", filepath.Join(outside, "x.go")},
	} {
		ui.asked = nil
		pipe.Check(tools.Request{Subject: "fs.write", Arg: filepath.Join(proj, c.arg)})
		if len(ui.asked) != 1 || ui.asked[0].Arg != perm.TildePath(c.shown) {
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
		if req.arg == "rm -rf /" or req.agent == "explore agent" then return "deny" end
		if req.arg ~= "cat x" then return "allow" end
	end })`)
	if err := h.SetPermMode("review"); err != nil {
		t.Fatal(err)
	}
	ask := h.askHook()
	for _, c := range []struct {
		name string
		req  tools.Request
		want bool
		by   string
		why  DenialWhy // the denial noted, "" for none
	}{
		{"allowed", tools.Request{Subject: "tool.shell", Arg: "ls"}, true, "mode:review", ""},
		{"denied", tools.Request{Subject: "tool.shell", Arg: "rm -rf /"}, false, "mode:review", ""},
		{"a subagent's", tools.Request{Subject: "tool.shell", Arg: "ls", Agent: "explore agent"}, false, "mode:review", ""},
		{"left to the user", tools.Request{Subject: "tool.shell", Arg: "cat x"}, false, tools.ByNobody, DenyApproval},
		// what the mode's own rules may not settle, decide may not either
		{"a rule asked for it", tools.Request{Subject: "tool.shell", Arg: "ls", Explicit: true}, false, tools.ByNobody, DenyApproval},
		{"a protected write", tools.Request{Subject: "fs.write", Arg: filepath.Join(h.Dirs.ProjectDir, ".git", "hooks", "pre-commit")}, false, tools.ByNobody, DenyApproval},
		{"unsandboxed", tools.Request{Tool: "shell", Subject: "tool.shell", Arg: "ls", Unsandboxed: true}, false, tools.ByNobody, DenyOutsideSandbox},
		{"network", tools.Request{Tool: "shell", Subject: "tool.shell", Arg: "curl x", Network: true}, false, tools.ByNobody, DenyNetwork},
		{"a lua script", tools.Request{Tool: tools.ToolLua, Subject: "tool.lua", Arg: "zemi.x()", Unsandboxed: true}, false, tools.ByNobody, DenyNeedsTUI},
	} {
		before := len(h.HeadlessDenials())
		ok, by, _ := ask(c.req)
		d := h.HeadlessDenials()[before:]
		if ok != c.want || by != c.by || (c.why == "") != (len(d) == 0) || len(d) > 0 && (d[0].Why != c.why || d[0].Subject != c.req.Subject) {
			t.Errorf("%s: allowed %v by %s, noted %+v", c.name, ok, by, d)
		}
	}
}

// TestModeDecideStaleGeneration: the ask copies the mode def before the
// reload window closes, so a purge moves the list out from under the copied
// Decide; it must not answer for the new generation.
func TestModeDecideStaleGeneration(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `zemi.permission.mode.add("review", { base = "yolo", decide = function() return "allow" end })`)
	if err := h.SetPermMode("review"); err != nil {
		t.Fatal(err)
	}
	def := h.permModeDef()
	if def.Decide == nil {
		t.Fatal("the mode has no decide")
	}
	if err := r.ForceReload(t.Context(), []lua.BootStep{h.NamespacesStep()}, h.PurgeGeneration); err != nil {
		t.Fatal(err)
	}
	if got := h.modeDecide(def, tools.Request{Subject: "tool.shell", Arg: "ls"}); got != "" {
		t.Fatalf("a stale decide answered %q", got)
	}
}

// TestAskWaitsOutReload: mid-reload the config's modes are purged, so an
// ask judged then, like one that waited behind a prompt, waits for the
// replay instead of falling back to auto.
func TestAskWaitsOutReload(t *testing.T) {
	dir, log := t.TempDir(), slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dir, Origin: lua.OriginUser}}})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
	h.SetProjectTrusted(true)
	fakeSandbox(h, true)
	initLua := filepath.Join(dir, "init.lua")
	if err := os.WriteFile(initLua, []byte(`zemi.permission.mode.add("review", { base = "auto", decide = function() end })`), 0o600); err != nil {
		t.Fatal(err)
	}
	steps := []lua.BootStep{h.NamespacesStep(), lua.LoadFileStep("init", initLua, lua.OriginUser)}
	if err := r.Boot(t.Context(), steps); err != nil {
		t.Fatal(err)
	}
	if err := h.SetPermMode("review"); err != nil {
		t.Fatal(err)
	}
	reloading, reloaded := make(chan struct{}), make(chan error)
	probe := lua.BootStep{Name: "probe", Run: func(context.Context, *lua.Runtime, *lua.Generation) error {
		close(reloading)
		time.Sleep(50 * time.Millisecond) // the waiter's judgement lands inside the replay
		return nil
	}}
	ui := &fakeUI{}
	ui.onAsk = func(PermRequest) (bool, string) {
		if len(ui.asked) == 1 {
			for h.AsksWaiting() < 2 {
				runtime.Gosched()
			}
			go func() { reloaded <- h.ForceReload(t.Context(), []lua.BootStep{steps[0], probe, steps[1]}) }()
			<-reloading
		}
		return false, "user:once"
	}
	h.SetUIHooks(ui)
	pipe := h.PipelineFor()
	done := make(chan struct{})
	go func() { pipe.Check(tools.Request{Subject: "tool.shell", Arg: "ls"}); close(done) }()
	for h.AsksWaiting() < 1 {
		runtime.Gosched()
	}
	second := pipe.Check(tools.Request{Subject: "tool.shell", Arg: "pwd"})
	<-done
	if err := <-reloaded; err != nil {
		t.Fatal(err)
	}
	if len(ui.asked) != 2 || second.DecidedBy != "user:once" {
		t.Fatalf("%d prompts, the waiter decided by %q", len(ui.asked), second.DecidedBy)
	}
}

// TestPermissionsOverlayRemovesAlways: a mis-keyed "always" answer is the
// only rule a user writes at runtime and the only one with no file to edit,
// so /permissions shows it and takes it back.
func TestPermissionsOverlayRemovesAlways(t *testing.T) {
	h, r := newTestHost(t)
	h.Dirs.StateHome = t.TempDir()
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	// one always answer writes both forms
	for _, p := range []string{"git push", "git push *"} {
		h.Perm.Add(perm.Rule{Subject: "tool.shell", Pattern: p, Decision: perm.Allow, Principal: "model"})
	}
	h.Perm.Add(perm.Rule{Subject: "tool.shell", Pattern: "rm -rf *", Decision: perm.Deny})
	h.Perm.Add(perm.Rule{Subject: "tool.shell", Pattern: "make *", Decision: perm.Allow, Source: "permissions"})
	h.Perm.Add(perm.Rule{Subject: "tool.shell", Pattern: "curl *", Decision: perm.Ask, Source: "project"})
	h.Perm.Add(perm.Rule{Subject: "lua.http.get", Pattern: "https://x/*", Decision: perm.Allow, Principal: "plugin:foo"})
	if err := h.OpenPermissions(); err != nil || len(ui.opened) != 1 {
		t.Fatalf("open: %v %+v", err, ui.opened)
	}
	rows := fmt.Sprint(ui.opened[0].Items)
	if strings.Count(rows, "text:tool.shell git push") != 1 || !strings.Contains(rows, "text:tool.shell git push *") || !strings.Contains(rows, "map[hotkey:x run:permissions forget tool.shell git push text:Delete]") || !strings.Contains(rows, "key:session section:") {
		t.Fatalf("the answer is not one row with a way back:\n%s", rows)
	}
	// each rule says where to edit it, under a section named like every other
	for _, want := range []string{"section:Allow rules", "section:Deny rules", "section:Mode text:auto", "/permissions; edit that file", "from the project's .agents/init.lua", "from config; edit permissions in your init.lua", "key:plugin:foo"} {
		if !strings.Contains(rows, want) {
			t.Fatalf("missing %q:\n%s", want, rows)
		}
	}
	if err := h.ForgetRule("tool.shell rm -rf *"); err == nil {
		t.Error("a config rule must not be removable: a reload brings it back")
	}
	if err := h.ForgetRule("tool.shell git push"); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"git push", "git push origin main"} {
		if h.Perm.Evaluate("tool.shell", cmd, "model").Decision != perm.Ask {
			t.Errorf("%s: half of the removed answer still allows", cmd)
		}
	}
	// the row's value is the init.lua lines that bring the whole answer back
	var lines string
	for _, it := range ui.opened[0].Items {
		if m := it.(map[string]any); m["key"] == "session" {
			lines, _ = m["copy"].(string)
		}
	}
	runUser(t, r, lines)
	for _, cmd := range []string{"git push", "git push origin main"} {
		if h.Perm.Evaluate("tool.shell", cmd, "model").Decision != perm.Allow {
			t.Errorf("%s: the copied lines do not allow it:\n%s", cmd, lines)
		}
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
	h.Perm.Add(perm.Rule{Subject: "tool.shell", Pattern: "go test*", Decision: perm.Ask})
	h.Perm.Add(perm.Rule{Subject: "fs.read", Pattern: filepath.Join(proj, "*"), Decision: perm.Allow, NotMatch: filepath.Join(proj, "vendor", "*")})
	p := h.PipelineFor()
	for _, c := range []struct {
		subject, arg string
		settled      bool
	}{
		{"tool.shell", "go test ./...", false},
		{"tool.shell", "go vet ./...", true},
		{"fs.read", filepath.Join(proj, "vendor", "x.go"), false},
	} {
		v := p.Check(tools.Request{Subject: c.subject, Arg: c.arg})
		// no UI: an ask the mode leaves to the user is denied by default
		if got := v.Decision == perm.Allow; got != c.settled || !c.settled && !strings.Contains(v.Reason, "a rule asks for this") {
			t.Errorf("%s %q: %+v", c.subject, c.arg, v)
		}
	}
}

// TestAskWaiterJudgedAgain: asks reach the user one at a time, and one
// that waited behind a prompt is judged again (rules, mode) once the prompt
// is answered, so an answer that covers it settles it without a second
// prompt, with the rule's verdict whole.
func TestAskWaiterJudgedAgain(t *testing.T) {
	root := outsideTemp(t)
	for _, d := range []string{".git", "a", "b"} {
		os.Mkdir(filepath.Join(root, d), 0o700)
	}
	read := func(rel string) tools.Request {
		p := filepath.Join(root, rel)
		os.WriteFile(p, nil, 0o600)
		return tools.Request{Subject: "fs.read", Arg: p}
	}
	curl := tools.Request{Subject: "tool.shell", Arg: "curl example.com"}
	netCurl := curl
	netCurl.Network = true
	for _, c := range []struct {
		name          string
		first, second tools.Request
		by            string        // the first prompt's answer
		during        func(h *Host) // what happens while it is open
		prompts       int
		allowed       [2]bool
		secondBy      string
		network       bool // the second runs confined with the network
	}{
		{"always covers the waiting read", read("a/x"), read("a/y"), "user:always", nil, 1, [2]bool{true, true}, tools.ByRule, false},
		{"wider covers another package", read("a/x"), read("b/z"), "user:wider", nil, 1, [2]bool{true, true}, tools.ByRule, false},
		{"answer does not cover the waiter", read("a/x"), read("b/z"), "user:always", nil, 2, [2]bool{true, true}, "user:always", false},
		{"yolo switch settles the waiter", read("a/x"), read("a/y"), "user:once", func(h *Host) { h.SetPermMode("yolo") }, 1, [2]bool{true, true}, tools.ByMode("yolo"), false},
		{"tell denies the waiter", read("a/x"), read("a/y"), tools.ByUser, func(h *Host) { h.DenyWaitingAsks() }, 1, [2]bool{false, false}, tools.ByUser, false},
		{"network always-rule keeps the sandbox", netCurl, curl, "user:always", nil, 1, [2]bool{true, true}, tools.ByRule, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestHost(t)
			fakeSandbox(h, true)
			opened := make(chan struct{})
			ui := &fakeUI{}
			ui.onAsk = func(PermRequest) (bool, string) {
				if len(ui.asked) == 1 {
					close(opened)
					// the second is at the prompt stage; t.Error, since this is no test goroutine
					for deadline := time.Now().Add(3 * time.Second); h.AsksWaiting() < 2; runtime.Gosched() {
						if time.Now().After(deadline) {
							t.Error("the second ask never reached the prompt stage")
							break
						}
					}
					if c.during != nil {
						c.during(h)
					}
				}
				return c.by != tools.ByUser, c.by
			}
			h.SetUIHooks(ui)
			pipe := h.PipelineFor()
			var first tools.Verdict
			done := make(chan struct{})
			go func() { first = pipe.Check(c.first); close(done) }()
			<-opened
			second := pipe.Check(c.second)
			<-done
			if got := [2]bool{first.Decision == perm.Allow, second.Decision == perm.Allow}; got != c.allowed || len(ui.asked) != c.prompts || second.DecidedBy != c.secondBy {
				t.Fatalf("allowed %v, %d prompts, second by %q", got, len(ui.asked), second.DecidedBy)
			}
			if pol := pipe.SandboxFor(c.second, second); (pol != nil && pol.Network) != c.network {
				t.Fatalf("sandbox %+v", pol)
			}
		})
	}
}

// TestYoloToggle: the shipped ctrl+y reads the mode through get() and
// returns to the mode it left, a user mode included; a mode built on yolo
// counts as yolo.
func TestYoloToggle(t *testing.T) {
	for _, c := range []struct {
		name, setup string
		want        []string // get().name first, then after each press
	}{
		{"auto flips to yolo and back", ``, []string{"auto", "yolo", "auto"}},
		{"a user mode comes back after yolo", `zemi.permission.mode.add("review", { base = "auto" })
			zemi.permission.mode.set("review")`, []string{"review", "yolo", "review"}},
		{"a yolo-based user mode leaves yolo", `zemi.permission.mode.add("fast", { base = "yolo" })
			zemi.permission.mode.set("fast")`, []string{"fast", "auto"}},
		{"an unknown permissions.mode reads as auto", `zemi.config.set("permissions.mode", "gone")
			assert(zemi.config.get("permissions.mode") == "gone")`, []string{"auto"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newTestHostUI(t)
			runUser(t, h.RT, c.setup)
			for i, want := range c.want {
				if i > 0 {
					if err := h.RunRHS(t.Context(), h.Keymaps.Lookup("", "ctrl+y")); err != nil {
						t.Fatal(err)
					}
				}
				runUser(t, h.RT, fmt.Sprintf(`local m = zemi.permission.mode.get()
					assert(m.name == %q, "mode " .. m.name)
					for _, row in ipairs(zemi.permission.mode.list()) do assert(row.active == nil) end`, want))
			}
		})
	}
}
