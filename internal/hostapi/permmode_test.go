package hostapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/brain"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
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
		if req.arg == "slow" then while true do end end
		if req.arg == "boom" then error("the judge broke") end
		if req.arg == "rm -rf /" or req.agent == "explore agent" then return "deny" end
		if req.arg ~= "cat x" then return "allow" end
	end })`)
	if err := h.SetPermMode("review"); err != nil {
		t.Fatal(err)
	}
	ask := h.askHook()
	for _, c := range []struct {
		name   string
		req    tools.Request
		want   bool
		by     string
		why    DenialWhy // the denial noted, "" for none
		notice bool      // the row a fenced allow leaves in the transcript
	}{
		{"allowed", tools.Request{Subject: "tool.shell", Arg: "ls"}, true, "mode:review", "", false},
		{"denied", tools.Request{Subject: "tool.shell", Arg: "rm -rf /"}, false, "mode:review", "", false},
		{"a subagent's", tools.Request{Subject: "tool.shell", Arg: "ls", Agent: "explore agent"}, false, "mode:review", "", false},
		{"left to the user", tools.Request{Subject: "tool.shell", Arg: "cat x"}, false, tools.ByNobody, DenyApproval, false},
		{"a decide that raised", tools.Request{Subject: "tool.shell", Arg: "boom"}, false, tools.ByNobody, DenyApproval, false},
		// what the mode's own rules may not settle a decide may, since a model
		// call is what writing the mode asked for; each leaves a row saying so
		{"a rule asked for it", tools.Request{Subject: "tool.shell", Arg: "ls", Explicit: true}, true, "mode:review", "", true},
		{"a protected write", tools.Request{Subject: "fs.write", Arg: filepath.Join(h.Dirs.ProjectDir, ".git", "hooks", "pre-commit")}, true, "mode:review", "", true},
		{"unsandboxed", tools.Request{Tool: "shell", Subject: "tool.shell", Arg: "ls", Unsandboxed: true}, true, "mode:review", "", true},
		{"network", tools.Request{Tool: "shell", Subject: "tool.shell", Arg: "curl x", Network: true}, true, "mode:review", "", true},
		{"a lua script", tools.Request{Tool: tools.ToolLua, Subject: "tool.lua", Arg: "zemi.x()", Unsandboxed: true}, true, "mode:review", "", true},
	} {
		before, beforeD := len(h.Session().Transcript), len(h.HeadlessDenials())
		ok, by, _ := ask(c.req)
		d := h.HeadlessDenials()[beforeD:]
		noticed := slices.ContainsFunc(h.Session().Transcript[before:], func(m ChatMessage) bool { return m.Kind == RowNotice })
		if ok != c.want || by != c.by || (c.why == "") != (len(d) == 0) || len(d) > 0 && (d[0].Why != c.why || d[0].Subject != c.req.Subject) || noticed != c.notice {
			t.Errorf("%s: allowed %v by %s, noted %+v, notice %v", c.name, ok, by, d, noticed)
		}
	}
	// a decide that outlives its ceiling leaves the ask to the user: the path
	// where dropping the settleable gate could fail open
	long := modeDecideCeiling
	modeDecideCeiling = 100 * time.Millisecond
	defer func() { modeDecideCeiling = long }()
	before := len(h.HeadlessDenials())
	start := time.Now()
	ok, by, _ := ask(tools.Request{Subject: "tool.shell", Arg: "slow"})
	d := h.HeadlessDenials()[before:]
	if ok || by != tools.ByNobody || len(d) != 1 || d[0].Why != DenyApproval {
		t.Fatalf("a decide past its ceiling: allowed %v by %s, noted %+v", ok, by, d)
	}
	if elapsed := time.Since(start); elapsed < modeDecideCeiling {
		t.Fatalf("the decide answered in %v, so the ceiling never fired", elapsed)
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
	if got := h.modeDecide(def, nil); got.Decision != "" {
		t.Fatalf("a stale decide answered %q", got)
	}
}

// TestAskWaitsOutReload: mid-reload the config's modes are purged, so an
// ask judged then, like one that waited behind a prompt, waits for the
// replay instead of falling back to auto.
func TestAskWaitsOutReload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir, log := t.TempDir(), slog.New(slog.DiscardHandler)
		r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dir, Origin: lua.OriginUser}}})
		h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
		t.Cleanup(h.Close)
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
				// the second ask must reach the prompt before the reload; the
				// bound fails the test here instead of hanging the package
				for i := 0; h.AsksWaiting() < 2 && i < 1_000_000; i++ {
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
		for i := 0; h.AsksWaiting() < 1 && i < 1_000_000; i++ {
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
	})
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

// TestBrainWritesProtected: a file-tool write to a state-dir brain path is a
// protected ask no store rule or mode rule settles, in every mode except yolo;
// a decide settles it, as the user could, and the row it leaves says so. The
// reserved file names its owner, a freeform file zemi's own files (DESIGN
// §10.4). The lsp rename asks like the write it is.
func TestBrainWritesProtected(t *testing.T) {
	h, r := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	proj := perm.RealPath(h.Dirs.ProjectDir)
	user, private := h.brainParts()
	userMem := filepath.Join(perm.RealPath(user.Dir), brain.MemFile)
	userFile := filepath.Join(perm.RealPath(user.Dir), "style.md")
	privateMem := filepath.Join(perm.RealPath(private.Dir), brain.MemFile)
	privateFile := filepath.Join(perm.RealPath(private.Dir), "repo-map.md")
	for _, root := range []string{h.Dirs.StateHome, h.Dirs.DataHome, proj} {
		h.Perm.Add(perm.Rule{Subject: "fs.write", Pattern: filepath.Join(perm.RealPath(root), "*"), Decision: perm.Allow, Principal: "model"})
	}
	runUser(t, r, `zemi.permission.mode.add("open", { base = "auto", rules = { { "fs.write", "*", "allow" } } })`)
	runUser(t, r, `zemi.permission.mode.add("decider", { base = "auto", decide = function() return "allow" end })`)
	// the rename's write request, shaped as lspRequests builds it
	rename := tools.Request{Subject: "fs.write", Arg: userMem, Reason: "rename a to b"}
	pipe := h.PipelineFor()
	for _, c := range []struct {
		name, mode, why string
		req             tools.Request
		want            perm.Decision
	}{
		{"user memories.md", "auto", "the memory tool owns this file", tools.Request{Subject: "fs.write", Arg: userMem}, perm.Deny},
		{"user freeform", "auto", "zemi's own files", tools.Request{Subject: "fs.write", Arg: userFile}, perm.Deny},
		{"private memories.md", "auto", "the memory tool owns this file", tools.Request{Subject: "fs.write", Arg: privateMem}, perm.Deny},
		{"private freeform", "auto", "zemi's own files", tools.Request{Subject: "fs.write", Arg: privateFile}, perm.Deny},
		{"mode rule", "open", "the memory tool owns this file", tools.Request{Subject: "fs.write", Arg: userMem}, perm.Deny},
		{"decide", "decider", "", tools.Request{Subject: "fs.write", Arg: userMem}, perm.Allow},
		{"lsp rename", "auto", "the memory tool owns this file", rename, perm.Deny},
	} {
		if err := h.SetPermMode(c.mode); err != nil {
			t.Fatal(err)
		}
		v := pipe.Check(c.req)
		if v.Decision != c.want || c.why != "" && !strings.Contains(v.Reason, c.why) {
			t.Errorf("%s (%s): %s %q, want %s naming %s", c.name, c.mode, v.Decision, v.Reason, c.want, c.why)
		}
	}
}

// TestBrainAskNotes: a brain ask notes outside text only when the session
// read any, and a memories.md ask carries the file's open lint findings, at
// most three (DESIGN §10.4).
func TestBrainAskNotes(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()
	ui := &fakeUI{askAllowed: true, askBy: "user:once"}
	h.SetUIHooks(ui)
	if err := h.SetPermMode("auto"); err != nil {
		t.Fatal(err)
	}
	sid := seedSession(t, h)
	user, _ := h.brainParts()
	mem := filepath.Join(user.Dir, brain.MemFile)
	memDir := filepath.Dir(mem)
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// four malformed lines: three findings ride the ask, the fourth is cut;
	// a bad freeform file lints too, and its findings are not the file's
	if err := os.WriteFile(mem, []byte("one\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "style.md"), []byte("---\nread_if: r\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ask := func(arg string) string {
		before := len(ui.asked)
		if ok, _, _ := h.askHook()(tools.Request{Subject: "fs.write", Arg: arg}); !ok {
			t.Fatalf("%s: the ask was not allowed", arg)
		}
		if len(ui.asked) != before+1 {
			t.Fatalf("%s: asked %+v", arg, ui.asked)
		}
		return ui.asked[before].Reason
	}
	local := ask(mem)
	if !strings.Contains(local, "the memory tool owns this file") ||
		!strings.Contains(local, "line 3 is not a memory line") ||
		strings.Contains(local, "line 4 is not a memory line") ||
		strings.Contains(local, "no description") ||
		strings.Contains(local, "outside the machine") {
		t.Errorf("a local-only session's memories.md ask: %q", local)
	}
	if r := ask(filepath.Join(memDir, "style.md")); strings.Contains(r, "outside the machine") || strings.Contains(r, "not a memory line") {
		t.Errorf("a freeform brain ask: %q", r)
	}
	h.recordToolCall(sid, StreamTool{ID: "c1", Name: "fetch"}, true)
	if r := ask(mem); !strings.Contains(r, "outside the machine") {
		t.Errorf("a session that fetched: %q", r)
	}
}

// TestModeOwnership: mode names follow the registry's origins. The builtins are
// reserved by name, since one mode.add("auto", {base = "yolo"}) would hand away
// the mode every later boot falls back on; a holder a less trusted origin may
// not replace, two plugins are told apart by their file, and the user may still
// take a plugin's name back.
func TestModeOwnership(t *testing.T) {
	h, _ := newTestHost(t)
	plugin := func(name string) string { return filepath.Join(h.pluginsRoot(), "local", name, "init.lua") }
	add := func(name, base string) string {
		return fmt.Sprintf(`zemi.permission.mode.add(%q, { base = %q })`, name, base)
	}
	for _, c := range []struct {
		name, src, path string
		origin          lua.Origin
		want            string // what the refusal names, "" when it must be taken
	}{
		{"a user may not take the auto name", add("auto", "yolo"), "u.lua", lua.OriginUser, "is a builtin mode"},
		{"nor yolo's", add("yolo", "auto"), "u.lua", lua.OriginUser, "is a builtin mode"},
		{"a plugin may not either", add("auto", "yolo"), plugin("a"), lua.OriginPlugin, "is a builtin mode"},
		{"a plugin registers its own mode", add("p", "auto"), plugin("a"), lua.OriginPlugin, ""},
		{"another plugin may not replace it", add("p", "yolo"), plugin("b"), lua.OriginPlugin, "is registered by plugin a, not b"},
		{"nor drop it", `zemi.permission.mode.del("p")`, plugin("b"), lua.OriginPlugin, "is registered by plugin a, not b"},
		{"a user may", add("p", "yolo"), "u.lua", lua.OriginUser, ""},
		{"and the plugin may not take it back", add("p", "auto"), plugin("a"), lua.OriginPlugin, "is registered by user config"},
		{"a builtin name is refused a del too", `zemi.permission.mode.del("auto")`, "u.lua", lua.OriginUser, "is a builtin mode"},
		// a mode's rules match every principal, so an allow widens as far as
		// rule.add's and needs user config; a plugin keeps deny and ask
		{"a plugin's rule may not allow", `zemi.permission.mode.add("r", { base = "auto", rules = { { "tool.shell", "ls *", "allow" } } })`, plugin("a"), lua.OriginPlugin, "allow rules may only come from user config"},
		{"a plugin's deny rule is taken", `zemi.permission.mode.add("r2", { base = "auto", rules = { { "tool.shell", "ls *", "deny" } } })`, plugin("a"), lua.OriginPlugin, ""},
		{"a user's allow rule is taken", `zemi.permission.mode.add("r", { base = "auto", rules = { { "tool.shell", "ls *", "allow" } } })`, "u.lua", lua.OriginUser, ""},
	} {
		err := h.RT.ExecuteChunk(t.Context(), c.src, c.path, c.origin)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	if err := h.SetPermMode("p"); err != nil {
		t.Fatal(err)
	}
	if got := h.permModeDef(); got.Origin != lua.OriginUser || got.Base != "yolo" {
		t.Fatalf("the active mode is %+v, want the user's def", got)
	}
	// a plugin's own mode is selectable like any other
	if err := h.RT.ExecuteChunk(t.Context(), add("q", "auto"), plugin("a"), lua.OriginPlugin); err != nil {
		t.Fatal(err)
	}
	if err := h.SetPermMode("q"); err != nil {
		t.Fatal(err)
	}
	if got := h.permModeDef(); got.Name != "q" || got.Origin != lua.OriginPlugin || got.Path != plugin("a") {
		t.Fatalf("mode q = %+v", got)
	}
	// a mode the setting names and config then drops falls back to the default,
	// not to whatever the list happens to hold first
	runUser(t, h.RT, `zemi.permission.mode.del("q")`)
	if got := h.PermMode(); got != config.PermModeAuto {
		t.Fatalf("the mode after the active one was dropped = %q", got)
	}
}

// TestModePickDroppedWhenYolo: a mode picked while it was auto-based must not
// start a later boot in that mode once its base turns yolo. keepPick refuses to
// store a yolo pick and loadPicks drops a literal yolo row, both before any Lua
// runs, and neither knows a mode that was auto-based when it was picked; the
// registration site is where the base becomes known, so a fresh boot re-checks
// too, not only a replay.
func TestModePickDroppedWhenYolo(t *testing.T) {
	dir, log := t.TempDir(), slog.New(slog.DiscardHandler)
	dirs := config.Dirs{ProjectDir: t.TempDir(), StateHome: t.TempDir()}
	initLua := filepath.Join(dir, "init.lua")
	// the mode as the config defines it, and the pick /mode kept while it was
	// auto-based, before the boot that must re-check the base
	writeInit := func(base string) {
		t.Helper()
		if err := os.WriteFile(initLua, []byte(fmt.Sprintf(`zemi.permission.mode.add("x", { base = %q })`, base)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var h *Host
	steps := func() []lua.BootStep {
		return []lua.BootStep{h.NamespacesStep(), lua.LoadFileStep("init", initLua, lua.OriginUser)}
	}
	start := func() {
		r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: dir, Origin: lua.OriginUser}}})
		h = NewHost(r, log, config.NewStore(), dirs)
		t.Cleanup(h.Close)
		if err := r.Boot(t.Context(), steps()); err != nil {
			t.Fatal(err)
		}
	}
	writeInit("auto")
	start()
	if err := h.GlobalDB.SetKV("config:permissions.mode", `"x"`); err != nil {
		t.Fatal(err)
	}
	start()
	if got := h.PermMode(); got != "x" {
		t.Fatalf("an auto-based mode's pick did not apply: %q", got)
	}
	if pick, _ := h.picked["permissions.mode"]; pick != "x" {
		t.Fatalf("the first boot kept no pick: %v", pick)
	}
	// the plugin updates the mode to a yolo base, the pick still naming it
	writeInit("yolo")
	if err := h.GlobalDB.SetKV("config:permissions.mode", `"x"`); err != nil {
		t.Fatal(err)
	}
	start()
	if got := h.PermMode(); got != config.PermModeAuto {
		t.Fatalf("a yolo-based mode started active: %q", got)
	}
	if _, ok := h.picked["permissions.mode"]; ok {
		t.Error("the pick is still held")
	}
	if _, err := h.GlobalDB.GetKV("config:permissions.mode"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the pick row is still stored (%v)", err)
	}
	// config writing the same name is a choice of its own: the pick row goes,
	// the setting stays
	if err := os.WriteFile(initLua, []byte(`zemi.config.set("permissions.mode", "x")
zemi.permission.mode.add("x", { base = "yolo" })`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.GlobalDB.SetKV("config:permissions.mode", `"x"`); err != nil {
		t.Fatal(err)
	}
	start()
	if got := h.PermMode(); got != "x" {
		t.Fatalf("config's own choice was reset: %q", got)
	}
	if _, err := h.GlobalDB.GetKV("config:permissions.mode"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the pick row is still stored (%v)", err)
	}
	// a reload keeps the pick's value live (a replay skips a write matching the
	// last load), so the re-check reads config's own value, not whether config
	// wrote the key at all: the yolo-based mode must not stay active
	if err := os.WriteFile(initLua, []byte(`zemi.config.set("permissions.mode", "auto")
zemi.permission.mode.add("x", { base = "auto" })`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.GlobalDB.SetKV("config:permissions.mode", `"x"`); err != nil {
		t.Fatal(err)
	}
	start()
	if err := h.GlobalDB.SetKV("config:permissions.mode", `"x"`); err != nil {
		t.Fatal(err)
	}
	h.KeepPicks.Store(true)
	if err := h.SetPermMode("x"); err != nil { // the pick the user made while it was auto-based
		t.Fatal(err)
	}
	if pick, _ := h.picked["permissions.mode"]; pick != "x" {
		t.Fatalf("the runtime pick was not kept: %v", pick)
	}
	if err := os.WriteFile(initLua, []byte(`zemi.config.set("permissions.mode", "auto")
zemi.permission.mode.add("x", { base = "yolo" })`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.ForceReload(t.Context(), steps()); err != nil {
		t.Fatal(err)
	}
	if got := h.PermMode(); got != config.PermModeAuto {
		t.Fatalf("a yolo-based mode stayed active through a reload: %q", got)
	}
	if _, ok := h.picked["permissions.mode"]; ok {
		t.Error("the pick is still held after the reload")
	}
	if _, err := h.GlobalDB.GetKV("config:permissions.mode"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the pick row is still stored after the reload (%v)", err)
	}
}

// TestModeDecideScopes: a decide's table answer names the decision, how far it
// goes and why; the scope writes what the user's own answer of that scope
// writes, and one this request does not offer narrows to the closest below it.
func TestModeDecideScopes(t *testing.T) {
	h, r := newTestHost(t)
	h.Dirs.StateHome = t.TempDir()
	h.SetProjectTrusted(true)
	runUser(t, r, `zemi.permission.mode.add("judge", {
		base = "auto",
		rules = { { "tool.shell", "git push *", "deny" } },
		decide = function(req)
			if req.protected_write then return { decision = "deny", reason = "protected: " .. req.protected_why } end
			if req.subject == "fs.read" then
				if req.arg:match("advise") then return { decision = "deny", reason = "options: " .. table.concat(req.options, ",") } end
				return { decision = "allow", scope = "wider", reason = "widest on offer" }
			end
			if req.arg == "rm -rf /" then return { decision = "deny", reason = "that deletes the machine" } end
			if req.arg == "cargo build" then return { decision = "allow", scope = "project", reason = "routine" } end
			if req.arg == "go test ./..." then return { decision = "allow", scope = "session", reason = "routine" } end
			return { decision = "allow", scope = "wider", reason = "widest on offer" }
		end })`)
	if err := h.SetPermMode("judge"); err != nil {
		t.Fatal(err)
	}
	read := filepath.Join(h.Dirs.ProjectDir, "a.go")
	for _, c := range []struct {
		name    string
		req     tools.Request
		ok      bool
		by, why string // why is what a denial hands back, "" when it need not match
		then    func(t *testing.T)
	}{
		{
			"a session scope writes the always rules",
			tools.Request{Subject: perm.SubjectShell, Arg: "go test ./..."},
			true, "mode:judge", "",
			func(t *testing.T) {
				if got := h.Perm.CheckCommandFor(perm.SubjectShell, perm.Normalize("go test -run X ./pkg"), perm.PrincipalModel).Decision; got != perm.Allow {
					t.Errorf("go test rule: %v", got)
				}
			},
		},
		{
			"a project scope writes the project file",
			tools.Request{Subject: perm.SubjectShell, Arg: "cargo build"},
			true, "mode:judge", "",
			func(t *testing.T) {
				b, err := os.ReadFile(h.projectPermissionsPath())
				if err != nil || !strings.Contains(string(b), "cargo build") {
					t.Errorf("project permissions: %q, %v", b, err)
				}
			},
		},
		{
			"a scope not on offer narrows",
			tools.Request{Subject: perm.SubjectRead, Arg: read},
			true, "mode:judge", "",
			func(t *testing.T) {
				// "wider" needs a repository above the file: the directory rule stands
				abs := perm.RealPath(read)
				if got := h.Perm.Evaluate(perm.SubjectRead, abs, perm.PrincipalModel).Decision; got != perm.Allow {
					t.Errorf("the narrowed session rule: %v", got)
				}
			},
		},
		{
			"a rule's deny outranks the decide",
			tools.Request{Subject: perm.SubjectShell, Arg: "git push origin main"},
			false, "mode:judge", "",
			func(t *testing.T) {
				if got := h.Perm.CheckCommandFor(perm.SubjectShell, perm.Normalize("git push origin main"), perm.PrincipalModel).Decision; got == perm.Allow {
					t.Error("the deny rule's answer was widened")
				}
			},
		},
		{
			// the payload's own fields reach the decide: what no rule settles and
			// what an allow could take here
			"a protected write",
			tools.Request{Subject: perm.SubjectWrite, Arg: filepath.Join(h.Dirs.ProjectDir, ".git", "hooks", "pre-commit")},
			false, "mode:judge",
			"protected: this changes .git, which decides what git runs", nil,
		},
		{
			"and the scopes on offer",
			tools.Request{Subject: perm.SubjectRead, Arg: filepath.Join(h.Dirs.ProjectDir, "advise.go")},
			false, "mode:judge",
			"options: once,session", nil,
		},
		{
			"a decide's own reason reaches whoever reads the denial",
			tools.Request{Subject: perm.SubjectShell, Arg: "rm -rf /"},
			false, "mode:judge",
			"that deletes the machine", nil,
		},
	} {
		ok, by, why := h.askHook()(c.req)
		if ok != c.ok || by != c.by || c.why != "" && why != c.why {
			t.Errorf("%s: allowed %v by %s, %q", c.name, ok, by, why)
			continue
		}
		if c.then != nil {
			c.then(t)
		}
	}
	if h.Perm.CheckCommandFor(perm.SubjectShell, perm.Normalize("rm -rf /"), perm.PrincipalModel).Decision == perm.Allow {
		t.Error("a decided deny wrote a rule")
	}
}

// TestModeDecidePluginScopes: a decide below user origin settles the ask and
// writes session rules, but the durable project file is the user's own grant,
// as an allow rule is (rule.add): its "project" answer narrows to session.
func TestModeDecidePluginScopes(t *testing.T) {
	h, _ := newTestHost(t)
	h.Dirs.StateHome = t.TempDir()
	h.SetProjectTrusted(true)
	path := filepath.Join(h.pluginsRoot(), "local", "judge", "init.lua")
	if err := h.RT.ExecuteChunk(t.Context(), `zemi.permission.mode.add("judge", { base = "auto", decide = function(req)
		if req.arg == "cargo build" then return { decision = "allow", scope = "project", reason = "routine" } end
		return { decision = "allow", scope = "session", reason = "routine" }
	end })`, path, lua.OriginPlugin); err != nil {
		t.Fatal(err)
	}
	if err := h.SetPermMode("judge"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, arg, rule string }{
		{"a session answer writes a session rule", "go test ./...", "go test -run X ./pkg"},
		{"a project answer narrows to session", "cargo build", "cargo build --release"},
	} {
		ok, by, _ := h.askHook()(tools.Request{Subject: perm.SubjectShell, Arg: c.arg})
		if !ok || by != "mode:judge" {
			t.Fatalf("%s: allowed %v by %s", c.name, ok, by)
		}
		if got := h.Perm.CheckCommandFor(perm.SubjectShell, perm.Normalize(c.rule), perm.PrincipalModel).Decision; got != perm.Allow {
			t.Errorf("%s: the session rule is %v", c.name, got)
		}
	}
	if b, err := os.ReadFile(h.projectPermissionsPath()); err == nil && strings.Contains(string(b), "cargo") {
		t.Errorf("a plugin's decide wrote the project file: %q", b)
	}
}

// TestPlanApprovalAfterModeGone: a mode the config dropped since the read-only
// agent was picked is no reason to read an approved plan as a dismissal. The
// restore skips it, the fallback mode stands, and the handoff is still recorded.
func TestPlanApprovalAfterModeGone(t *testing.T) {
	h, _ := newTestHost(t)
	ui := &fakeUI{planOK: true, planVerdict: tools.PlanVerdict{Approved: true}}
	h.SetUIHooks(ui)
	h.Settings.SetQuiet("permissions.mode", "gone") // the mode the agent was picked under, gone since
	if err := h.SetProfile("planner"); err != nil {
		t.Fatal(err)
	}
	v, ok := h.approvePlan("# The plan")
	if !ok || !v.Approved {
		t.Fatalf("approve: %+v ok=%v", v, ok)
	}
	if got := h.PermMode(); got != config.PermModeAuto {
		t.Fatalf("mode after the approval = %q, want the fallback", got)
	}
	if h.takePlanApproved() != implementNow {
		t.Fatal("the approval recorded no handoff")
	}
}
