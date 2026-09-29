package hostapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lsp"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// TestLSPDiagnostics: a server added from Lua reports on files the write
// tool changes, and the diagnostics tool reads them back.
func TestLSPDiagnostics(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*Host, func(tool, name, content string) string) {
		h, r := newTestHostDB(t)
		t.Cleanup(func() { h.lsp.stopAll() })
		h.SetProjectTrusted(true)
		runUser(t, r, fmt.Sprintf(`zemi.lsp.add("fake", { command = %q, env = { ZEMI_LSP_FAKE = "1" }, filetypes = { "txt", "quiet", "hang" } })`, os.Args[0]))
		h.Perm.Add(perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow})
		byName := map[string]fantasy.AgentTool{}
		for _, tl := range tools.All(h.PipelineFor()) {
			byName[tl.Info().Name] = tl
		}
		return h, func(tool, name, content string) string {
			t.Helper()
			in := map[string]string{"path": filepath.Join(h.Dirs.ProjectDir, name)}
			if tool == "write" {
				in["content"] = content
			}
			input, _ := json.Marshal(in)
			resp, err := byName[tool].Run(context.Background(), fantasy.ToolCall{ID: "1", Name: tool, Input: string(input)})
			if err != nil {
				t.Fatal(err)
			}
			return resp.Content
		}
	}
	t.Run("reports", func(t *testing.T) {
		t.Parallel()
		h, run := setup(t)
		for _, c := range []struct{ tool, name, content, want string }{
			{"write", "a.txt", "ok\nBROKEN", "error a.txt:2:1: broken"},
			{"diagnostics", "a.txt", "", `<diagnostics source="fake">`},
			{"write", "a.txt", "fixed", "fake reports no errors in a.txt"},
			{"write", "b.txt", "BROKEN", "error b.txt:1:1"},
			{"write", "a.txt", "still fine", "errors in other files (a caller this change broke, or already broken):\nerror b.txt:1:1: broken"},
			{"diagnostics", "b.zzz", "", "no language server for .zzz"},
		} {
			if got := run(c.tool, c.name, c.content); !strings.Contains(got, c.want) {
				t.Fatalf("%s %s = %q, want %q", c.tool, c.name, got, c.want)
			}
		}
		if row := fmt.Sprint(h.lspItems()); !strings.Contains(row, "[status ready]") || !strings.Contains(row, "hotkey:r run:lsp restart fake") {
			t.Fatalf("/lsp row = %s", row)
		}
	})
	// each silent touch waits out the client's floor, so the two run side by side
	for _, ext := range []string{"quiet", "hang"} {
		t.Run(ext, func(t *testing.T) {
			t.Parallel()
			h, run := setup(t)
			run("write", "a.txt", "ok")
			first := h.lsp.running["fake"]
			for i := range lspMaxStalls {
				run("write", fmt.Sprintf("q%d.%s", i, ext), "nothing")
			}
			if alive := h.lsp.running["fake"] == first; alive != (ext == "quiet") {
				t.Fatalf("only a server that answers nothing is dropped for a fresh start: %v", h.lspRows())
			}
			if got := run("write", "a.txt", "BROKEN"); !strings.Contains(got, "error a.txt:1:1") {
				t.Fatalf("restarted server = %q", got)
			}
		})
	}
}

// TestLSPTool: symbols, definition, references and rename go through the
// file's server by symbol name; rename writes every file the server names.
func TestLSPTool(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	defer func() { h.lsp.stopAll()() }()
	h.SetProjectTrusted(true)
	runUser(t, r, fmt.Sprintf(`zemi.lsp.add("fake", { command = %q, env = { ZEMI_LSP_FAKE = "1" }, filetypes = { "txt" } })`, os.Args[0]))
	h.Perm.Add(perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow})
	var tool fantasy.AgentTool
	for _, tl := range tools.All(h.PipelineFor()) {
		if tl.Info().Name == "lsp" {
			tool = tl
		}
	}
	a, b := filepath.Join(h.Dirs.ProjectDir, "a.txt"), filepath.Join(h.Dirs.ProjectDir, "b.txt")
	os.WriteFile(a, []byte("func Foo()\nFoo()\nfunc Bar()\n"), 0o644)
	os.WriteFile(b, []byte("x := Foo()\n"), 0o644)
	run := func(input string) string {
		t.Helper()
		resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "lsp", Input: input})
		if err != nil {
			t.Fatal(err)
		}
		// paths come back relative to the process working directory, which
		// is not the temp project here
		return strings.ReplaceAll(resp.Content, h.Dirs.ProjectDir+string(filepath.Separator), "")
	}
	// references list both lines, so no note names them again
	if got := run(fmt.Sprintf(`{"action":"references","path":%q,"symbol":"Foo"}`, a)); strings.Contains(got, "pass line") || !strings.Contains(got, "a.txt:2:1") {
		t.Fatalf("references repeat the lines they list: %q", got)
	}
	for _, c := range []struct{ input, want string }{
		{fmt.Sprintf(`{"action":"symbols","path":%q}`, a), "function Foo  lines 1-2\nfunction Bar  lines 3-4"},
		{fmt.Sprintf(`{"action":"definition","path":%q,"symbol":"Foo","line":2}`, a), "a.txt:1:6: func Foo()"},
		{fmt.Sprintf(`{"action":"definition","path":%q,"symbol":"Foo"}`, a), "also appears on line 2: pass line"},
		{fmt.Sprintf(`{"action":"references","path":%q,"symbol":"Foo"}`, b), "3 references\na.txt:1:6: func Foo()\na.txt:2:1: Foo()\nb.txt:1:6: x := Foo()"},
		{fmt.Sprintf(`{"action":"references","path":%q,"symbol":"Nope"}`, b), "does not appear"},
		{fmt.Sprintf(`{"action":"implementation","path":%q,"symbol":"Foo","line":2}`, a), "a.txt:1:6: func Foo()"},
		{fmt.Sprintf(`{"action":"workspace_symbols","path":%q,"symbol":"Ba"}`, b), "function Bar  a.txt:3"},
		{fmt.Sprintf(`{"action":"workspace_symbols","path":%q,"symbol":"Nope"}`, b), "no symbol matching Nope"},
		{fmt.Sprintf(`{"action":"call_hierarchy","path":%q,"symbol":"Foo","line":1}`, a), "callers of Foo:\n  b.txt:1: Foo\ncallees of Foo:\n  a.txt:3: Bar"},
		{fmt.Sprintf(`{"action":"call_hierarchy","path":%q,"symbol":"Foo","line":1,"direction":"incoming"}`, a), "callers of Foo:\n  b.txt:1: Foo"},
		{fmt.Sprintf(`{"action":"call_hierarchy","path":%q,"symbol":"Foo","line":1,"direction":"outgoing"}`, a), "callees of Foo:\n  a.txt:3: Bar"},
		{fmt.Sprintf(`{"action":"rename","path":%q,"symbol":"Foo","new_name":"Baz"}`, b), "renamed Foo to Baz in 2 files, 3 places"},
	} {
		if got := run(c.input); !strings.Contains(got, c.want) {
			t.Fatalf("%s = %q, want %q", c.input, got, c.want)
		}
	}
	if got, _ := os.ReadFile(a); string(got) != "func Baz()\nBaz()\nfunc Bar()\n" {
		t.Fatalf("a.txt after rename: %q", got)
	}
	if got, _ := os.ReadFile(b); string(got) != "x := Baz()\n" {
		t.Fatalf("b.txt after rename: %q", got)
	}
	// renaming straight back must see the files the first rename rewrote,
	// not the text the server was sent before it
	if got := run(fmt.Sprintf(`{"action":"rename","path":%q,"symbol":"Baz","new_name":"Foo"}`, b)); !strings.Contains(got, "renamed Baz to Foo in 2 files, 3 places") {
		t.Fatalf("reverse rename = %q", got)
	}
	if got, _ := os.ReadFile(a); string(got) != "func Foo()\nFoo()\nfunc Bar()\n" {
		t.Fatalf("a.txt after the reverse rename: %q", got)
	}
}

// TestLSPConfig: user config replaces a builtin by name with its own args
// and settings, disables one, and list shows what the project can use.
func TestLSPConfig(t *testing.T) {
	h, r := newTestHostDB(t)
	defer func() { h.lsp.stopAll()() }()
	runUser(t, r, `
		zemi.lsp.add("gopls", { command = "gopls-wrapper", args = { "-rpc", 1, true, {} }, env = { A = "x", PORT = 8080, V = false, T = {}, [1] = "N=1" }, filetypes = { "go" }, settings = { gopls = { staticcheck = true } }, init_options = { x = 1 } })
		zemi.lsp.add("zls", { command = "zls", filetypes = { "zig" } })
		zemi.lsp.del("zls")
	`)
	if defs := h.lspDefs(""); len(defs) != 0 {
		t.Fatalf("an untrusted project starts servers the user added: %+v", defs)
	}
	// with no server to talk to, the model does not carry their schemas
	lspTools := func() int {
		n := 0
		for _, tl := range h.profileTools(nil) {
			if name := tl.Info().Name; name == "lsp" || name == "diagnostics" {
				n++
			}
		}
		return n
	}
	if n := lspTools(); n != 0 {
		t.Fatalf("%d language server tools where none can start", n)
	}
	h.SetProjectTrusted(true)
	if n := lspTools(); n != 2 {
		t.Fatalf("language server tools with a server configured = %d", n)
	}
	defs := h.lspDefs("")
	if len(defs) != 1 || defs[0].Name != "gopls" || defs[0].Command != "gopls-wrapper" {
		t.Fatalf("defs = %+v", defs)
	}
	if !slices.Equal(defs[0].Args, []string{"-rpc", "1", "true"}) || !maps.Equal(defs[0].Env, map[string]string{"A": "x", "PORT": "8080", "V": "false"}) {
		t.Fatalf("scalars not read as strings: args = %q env = %q", defs[0].Args, defs[0].Env)
	}
	if cfg, _ := defs[0].Config.(map[string]any); cfg["gopls"].(map[string]any)["staticcheck"] != true || defs[0].Init.(map[string]any)["x"] != float64(1) {
		t.Fatalf("settings = %+v init = %+v", defs[0].Config, defs[0].Init)
	}
	if d, ok := h.lspFor("main.go"); !ok || d.Command != "gopls-wrapper" {
		t.Fatalf("lspFor(main.go) = %+v %v", d, ok)
	}
	runUser(t, r, `
		local rows = zemi.lsp.list()
		assert(#rows == 1 and rows[1].name == "gopls" and rows[1].status == "idle" and rows[1].filetypes == ".go", "list: " .. tostring(rows[1] and rows[1].status))
	`)
}

// TestLSPBuiltins: no builtin starts in an untrusted project; in a trusted
// one rust-analyzer does when a sandbox can confine it, and typescript only with a tsserver from outside
// the repository.
func TestLSPBuiltins(t *testing.T) {
	h, _ := newTestHostDB(t)
	root := h.Dirs.RepoRoot()
	global := t.TempDir()
	exe := func(path string) {
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755)
	}
	for _, f := range []string{"Cargo.toml", "package.json"} {
		os.WriteFile(filepath.Join(root, f), nil, 0o644)
	}
	for _, dir := range []string{global, root} {
		exe(filepath.Join(dir, "node_modules/typescript/bin/tsserver"))
		exe(filepath.Join(dir, "node_modules/typescript/lib/tsserver.js"))
		os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
		os.Symlink(filepath.Join(dir, "node_modules/typescript/bin/tsserver"), filepath.Join(dir, "bin/tsserver"))
	}
	servers := filepath.Join(global, "servers")
	exe(filepath.Join(servers, "rust-analyzer"))
	exe(filepath.Join(servers, "typescript-language-server"))
	for _, c := range []struct {
		name, path, want   string
		trusted, noSandbox bool
	}{
		{"untrusted", filepath.Join(global, "bin"), "", false, false},
		{"global tsserver", filepath.Join(global, "bin"), filepath.Join(global, "node_modules/typescript/lib/tsserver.js"), true, false},
		{"repo tsserver", filepath.Join(root, "bin"), "", true, false},
		{"no tsserver", "", "", true, false},
		{"no sandbox leaves rust-analyzer out", "", "", false, true},
	} {
		t.Setenv("PATH", servers+string(os.PathListSeparator)+c.path)
		h.SetProjectTrusted(c.trusted || c.noSandbox)
		fakeSandbox(h, !c.noSandbox)
		got, rust := "", false
		for _, d := range h.lspDefs("") {
			rust = rust || d.Name == "rust-analyzer"
			if d.Name == "typescript" {
				got = d.Init.(map[string]any)["tsserver"].(map[string]any)["path"].(string)
			}
		}
		if rust != c.trusted {
			t.Fatalf("%s: rust-analyzer listed = %v", c.name, rust)
		}
		want := c.want
		if want != "" {
			want, _ = filepath.EvalSymlinks(want)
		}
		if got != want {
			t.Fatalf("%s: tsserver = %q, want %q", c.name, got, want)
		}
	}
}

// TestLSPZemiLua: a .lua file of the user's config or a local plugin gets
// lua-language-server rooted at that tree, trusted project or not, with
// zemi's .luarc.json written first; an installed clone, a switched-off
// server and a file outside the repository get none, and the project's Lua
// gets the builtin only when trusted, like any builtin.
func TestLSPZemiLua(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "lua-language-server"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", bin)
	data, _ := filepath.EvalSymlinks(t.TempDir()) // plugin paths are resolved
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir(), StateHome: t.TempDir(), ConfigHome: t.TempDir(), DataHome: data})
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	cfg, proj, outside := h.Dirs.ConfigHome, h.Dirs.ProjectDir, t.TempDir()
	plugin := filepath.Join(h.Dirs.PluginsRoot(), "local", "stars")
	os.MkdirAll(filepath.Join(plugin, "lua", "stars"), 0o755) // made by the write the check follows
	os.WriteFile(filepath.Join(proj, ".luarc.json"), nil, 0o644)
	for _, c := range []struct {
		name, path, lua string // lua runs as user config first
		trusted         bool
		want, root      string // the server's name and root, "" for none
	}{
		{name: "config, untrusted", path: filepath.Join(cfg, "init.lua"), want: "luals (config)", root: cfg},
		{name: "a local plugin", path: filepath.Join(plugin, "lua", "stars", "init.lua"), want: "luals (plugin stars)", root: plugin},
		{name: "an installed clone", path: filepath.Join(h.Dirs.PluginsRoot(), "installed", "x", "init.lua"), trusted: true},
		{name: "a plugin's non-Lua file", path: filepath.Join(plugin, "README.md"), trusted: true},
		{name: "the project, untrusted", path: filepath.Join(proj, "a.lua")},
		{name: "the project, trusted", path: filepath.Join(proj, "a.lua"), trusted: true, want: "luals"},
		{name: "outside the repository", path: filepath.Join(outside, "a.lua"), trusted: true},
		{name: "lsp.auto off", path: filepath.Join(cfg, "init.lua"), lua: `zemi.config.set("lsp.auto", false)`},
		{name: "deleted", path: filepath.Join(cfg, "init.lua"), lua: `zemi.lsp.del("luals")`},
		{name: "a user's own", path: filepath.Join(plugin, "init.lua"), lua: `zemi.lsp.add("luals", { command = "my-luals", filetypes = { "lua" } })`, want: "luals (plugin stars)", root: plugin},
		{name: "a user's own without lua", path: filepath.Join(cfg, "init.lua"), lua: `zemi.lsp.add("luals", { command = "my-luals", filetypes = { "luau" } })`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h.lsp.stopAll()
			runUser(t, r, `zemi.config.set("lsp.auto", true) `+c.lua)
			h.SetProjectTrusted(c.trusted)
			os.RemoveAll(filepath.Join(c.root, ".luarc.json"))
			d, ok := h.lspFor(c.path)
			if ok != (c.want != "") || d.Name != c.want || d.Root != c.root {
				t.Fatalf("lspFor = %q rooted at %q (%v), want %q at %q", d.Name, d.Root, ok, c.want, c.root)
			}
			if _, err := os.Stat(filepath.Join(c.root, ".luarc.json")); c.root != "" && err != nil {
				t.Errorf("no .luarc.json before the server starts: %v", err)
			}
		})
	}
}

// TestLSPRealLuaLS runs the installed lua-language-server under the live
// backend on a local plugin in an untrusted project: zemi's types reach it,
// so a widget spec missing a field is an error, and the demo plugin, sound
// code, has none (the Error severities in zemi's .luarc.json).
func TestLSPRealLuaLS(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("starts a real language server")
	}
	if _, err := exec.LookPath("lua-language-server"); err != nil {
		t.Skip("lua-language-server not installed")
	}
	data, _ := filepath.EvalSymlinks(t.TempDir()) // plugin paths are resolved
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir(), StateHome: t.TempDir(), CacheHome: t.TempDir(), DataHome: data})
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	defer func() { h.lsp.stopAll()() }()
	if !h.sandboxLive() {
		t.Skip("no sandbox")
	}
	h.syncTypes()
	plugin := filepath.Join(h.Dirs.PluginsRoot(), "local", "demo")
	if err := os.CopyFS(plugin, os.DirFS("../../testdata/plugins/demo")); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(plugin, "lua", "bad.lua")
	var got string
	for i, deadline := 0, time.Now().Add(2*time.Minute); !strings.Contains(got, "error ") && time.Now().Before(deadline); i++ {
		// the server publishes a result only when it changed, so each try
		// moves the error a line down
		spec := strings.Repeat("\n", i) + "zemi.ui.widget.add({ id = \"x\", render = function() return {} end })\n"
		os.WriteFile(bad, []byte(spec), 0o644)
		got, _ = h.lspDiagnostics(bad)
	}
	if !strings.Contains(got, "error ") || !strings.Contains(got, "region") {
		t.Fatalf("the planted spec: %q", got)
	}
	// an edit that leaves the error as it was gets no new publish
	spec, _ := os.ReadFile(bad)
	os.WriteFile(bad, append(spec, "-- still broken\n"...), 0o644)
	if again, _ := h.lspDiagnostics(bad); again != got {
		t.Fatalf("after an edit that kept the error: %q, want %q", again, got)
	}
	err := filepath.WalkDir(plugin, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".lua" || p == bad {
			return err
		}
		// a file it finds nothing in it publishes nothing for, which reads as no answer
		if got, _ := h.lspDiagnostics(p); strings.Contains(got, "error ") {
			t.Errorf("%s: %q", p, got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestLSPSyncFromGit: files that appear, change or vanish on disk between
// two server interactions reach the server as watched-file events, found
// by diffing git status listings.
func TestLSPSyncFromGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	h, r := newTestHostDB(t)
	defer func() { h.lsp.stopAll()() }()
	h.SetProjectTrusted(true)
	runUser(t, r, fmt.Sprintf(`zemi.lsp.add("fake", { command = %q, env = { ZEMI_LSP_FAKE = "1" }, filetypes = { "txt" } })`, os.Args[0]))
	dir := h.Dirs.ProjectDir
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := h.git(dir, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	var tool fantasy.AgentTool
	for _, tl := range tools.All(h.PipelineFor()) {
		if tl.Info().Name == "lsp" {
			tool = tl
		}
	}
	a, b := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")
	os.WriteFile(a, []byte("func Foo()\nFoo()\n"), 0o644)
	refs := func() string {
		t.Helper()
		resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "lsp", Input: fmt.Sprintf(`{"action":"references","path":%q,"symbol":"Foo"}`, a)})
		if err != nil {
			t.Fatal(err)
		}
		return strings.SplitN(resp.Content, "\n", 2)[0]
	}
	if got := refs(); got != "2 references" {
		t.Fatalf("baseline: %q", got)
	}
	// each change is made behind the server's back, as a bash command
	// would, and marked dirty the way running one does
	os.WriteFile(b, []byte("x := Foo()\n"), 0o644)
	h.lspDirty()
	if got := refs(); got != "3 references" {
		t.Fatalf("after an untracked file appeared: %q", got)
	}
	os.WriteFile(b, []byte("Foo()\nFoo()\n"), 0o644)
	later := time.Now().Add(time.Hour)
	os.Chtimes(b, later, later)
	h.lspDirty()
	if got := refs(); got != "4 references" {
		t.Fatalf("after it changed: %q", got)
	}
	os.Remove(b)
	h.lspDirty()
	if got := refs(); got != "2 references" {
		t.Fatalf("after it was deleted: %q", got)
	}
	// with nothing marked, the sync is skipped entirely
	os.WriteFile(b, []byte("Foo()\n"), 0o644)
	if got := refs(); got != "2 references" {
		t.Fatalf("with no mark: %q", got)
	}
}

// TestLSPRealServersSandboxed runs the installed gopls, pyright and
// rust-analyzer under the live backend: each reports a problem, and the
// build script rust-analyzer runs writes its OUT_DIR but not outside it.
func TestLSPRealServersSandboxed(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("starts real language servers")
	}
	h, _ := newTestHostDB(t)
	// the temp dirs go once each server and the build it started have ended
	defer func() { h.lsp.stopAll()() }()
	if !h.sandboxLive() {
		t.Skip("no sandbox")
	}
	h.SetProjectTrusted(true)
	h.Dirs.CacheHome = t.TempDir()
	proj := h.Dirs.ProjectDir
	outside := filepath.Join(t.TempDir(), "escaped")
	files := map[string]string{
		"go.mod":         "module m\n\ngo 1.22\n",
		"main.go":        "package main\n\nfunc main() { var x int = \"s\"; _ = x }\n",
		"pyproject.toml": "[project]\nname = \"m\"\n",
		"a.py":           "x: int = \"s\"\n",
		"Cargo.toml":     "[package]\nname = \"m\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"build.rs": fmt.Sprintf("fn main() {\n    let _ = std::fs::write(%q, \"x\");\n"+
			"    std::fs::write(std::path::Path::new(&std::env::var(\"OUT_DIR\").unwrap()).join(\"ran\"), \"x\").unwrap();\n}\n", outside),
		"src/main.rs": "mod missing;\nfn main() {}\n",
	}
	for name, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(proj, name)), 0o755)
		os.WriteFile(filepath.Join(proj, name), []byte(body), 0o644)
	}
	// the confined cargo cannot write a missing Cargo.lock into the project
	if _, err := exec.LookPath("cargo"); err == nil {
		lock := exec.Command("cargo", "generate-lockfile")
		lock.Dir = proj
		if out, err := lock.CombinedOutput(); err != nil {
			t.Fatalf("cargo generate-lockfile: %v: %s", err, out)
		}
	}
	// the group returns once its parallel servers have all reported
	t.Run("servers", func(t *testing.T) {
		for _, c := range []struct {
			server, file string
			line         int
		}{
			{"gopls", "main.go", 3},
			{"pyright-langserver", "a.py", 1},
			{"rust-analyzer", "src/main.rs", 1},
		} {
			t.Run(c.server, func(t *testing.T) {
				t.Parallel()
				if _, err := exec.LookPath(c.server); err != nil {
					t.Skip(c.server, " not installed")
				}
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				defer cancel()
				path := filepath.Join(proj, c.file)
				s, err := h.lspOpen(ctx, path)
				if err != nil {
					t.Fatal(err)
				}
				// read what the server publishes for the open file: touching it
				// again waits a fixed floor a loaded server misses, and each miss
				// counts toward the stall that restarts it
				for !slices.ContainsFunc(s.Client.Diagnostics(path), func(d lsp.Diagnostic) bool { return d.Severity == 1 && d.Line == c.line }) {
					if ctx.Err() != nil {
						t.Fatalf("no error on line %d: %v", c.line, s.Client.Diagnostics(path))
					}
					time.Sleep(100 * time.Millisecond)
				}
			})
		}
	})
	if _, err := exec.LookPath("rust-analyzer"); err != nil {
		return
	}
	ran := func() bool {
		m, _ := filepath.Glob(filepath.Join(h.Dirs.CacheHome, "lsp", trustKey(h.trustRoot()), "rust-analyzer", "target", "*", "build", "*", "out", "ran"))
		return len(m) > 0
	}
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline) && !ran(); time.Sleep(time.Second) {
	}
	if !ran() {
		t.Fatal("rust-analyzer never ran build.rs")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("build.rs wrote outside its sandbox")
	}
}
