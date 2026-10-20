package lua

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	glua "github.com/yuin/gopher-lua"

	pluginpkg "github.com/metafates/zemi/internal/plugin"
)

// newRequireRuntime builds a runtime whose runtimepath covers a user config
// dir and a plugin dir (both symlink-resolved), with an empty embedded FS.
func newRequireRuntime(t *testing.T) (*Runtime, string, string) {
	t.Helper()
	resolve := func(dir string) string {
		p, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg := resolve(t.TempDir())
	plugin := resolve(t.TempDir())
	r := NewRuntime(RuntimeOptions{
		PathClass: PathClasses{
			{Prefix: cfg, Origin: OriginUser},
			{Prefix: plugin, Origin: OriginPlugin},
		},
		RuntimeFS: fstest.MapFS{},
	})
	if err := r.Boot(t.Context(), nil); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	return r, cfg, plugin
}

func writeFile(t *testing.T, path, src string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRequireResolvesRuntimepath(t *testing.T) {
	r, cfg, _ := newRequireRuntime(t)
	writeFile(t, filepath.Join(cfg, "lua", "mymod.lua"),
		"local M = {}\nM.x = 42\nreturn M\n")

	if err := r.ExecuteChunk(t.Context(), `
		local m = require("mymod")
		assert(m.x == 42, "module body did not run")
		assert(require("mymod") == m, "module result not cached")
	`, "init.lua", OriginUser); err != nil {
		t.Fatalf("chunk failed: %v", err)
	}
}

func TestRequireZemiNamespaceNotShadowable(t *testing.T) {
	r, cfg, _ := newRequireRuntime(t)
	// A user file placed exactly where require would look for zemi.evil:
	// zemi.* must resolve to the Go preload or embedded stdlib only.
	writeFile(t, filepath.Join(cfg, "lua", "zemi", "evil.lua"),
		"ZEMI_EVIL_LOADED = true\n")

	if err := r.ExecuteChunk(t.Context(), `
		local ok, err = pcall(require, "zemi.evil")
		assert(not ok, "user file shadowed a zemi.* module")
		assert(err:find("not found in embedded runtime", 1, true),
			"unexpected require error: " .. tostring(err))
		assert(ZEMI_EVIL_LOADED == nil, "shadow module body executed")
	`, "init.lua", OriginUser); err != nil {
		t.Fatalf("chunk failed: %v", err)
	}
}

func TestRequireCycleDetection(t *testing.T) {
	r, cfg, _ := newRequireRuntime(t)
	writeFile(t, filepath.Join(cfg, "lua", "cyc", "a.lua"), `require("cyc.b") return {}`)
	writeFile(t, filepath.Join(cfg, "lua", "cyc", "b.lua"), `require("cyc.a") return {}`)

	if err := r.ExecuteChunk(t.Context(), `
		local ok, err = pcall(require, "cyc.a")
		assert(not ok, "circular require succeeded")
		assert(err:find("circular require", 1, true),
			"unexpected require error: " .. tostring(err))
	`, "init.lua", OriginUser); err != nil {
		t.Fatalf("chunk failed: %v", err)
	}
}

// TestRequireRelative: "./x" resolves next to the requiring file (also from
// a function that runs later), shares the instance with the dotted name,
// and cannot reach another origin's tree.
func TestRequireRelative(t *testing.T) {
	r, cfg, plugin := newRequireRuntime(t)
	writeFile(t, filepath.Join(cfg, "lua", "app", "init.lua"), `return { util = require("./util") }`)
	writeFile(t, filepath.Join(cfg, "lua", "app", "util.lua"), `return { v = 1 }`)
	writeFile(t, filepath.Join(cfg, "lua", "app", "lazy.lua"), `return { get = function() return require("../late") end }`)
	writeFile(t, filepath.Join(cfg, "lua", "late.lua"), `return { v = 2 }`)
	writeFile(t, filepath.Join(plugin, "lua", "pmod.lua"), `return {}`)
	toPlugin, err := filepath.Rel(filepath.Join(cfg, "lua"), filepath.Join(plugin, "lua", "pmod"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cfg, "lua", "escape.lua"), `return require("./`+filepath.ToSlash(toPlugin)+`")`)

	if err := r.ExecuteChunk(t.Context(), `
		local app = require("./lua/app")
		assert(app.util.v == 1, "relative module did not load")
		assert(require("app.util") == app.util, "dotted and relative names loaded twice")
		assert(require("app.lazy").get().v == 2, "callback resolved against the wrong file")
		local ok, err = pcall(require, "escape")
		assert(not ok and err:find("outside the user tree", 1, true), "escaped the tree: " .. tostring(err))
	`, filepath.Join(cfg, "init.lua"), OriginUser); err != nil {
		t.Fatalf("chunk failed: %v", err)
	}
}

// TestRequireProjectModules: .agents/lua is on the path for every requirer.
func TestRequireProjectModules(t *testing.T) {
	proj, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := NewRuntime(RuntimeOptions{PathClass: PathClasses{{Prefix: proj, Origin: OriginProject}}, RuntimeFS: fstest.MapFS{}})
	if err := r.Boot(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(proj, "lua", "pmod.lua"), `return { v = 3 }`)
	if err := r.ExecuteChunk(t.Context(), `assert(require("pmod").v == 3)`, filepath.Join(proj, "init.lua"), OriginProject); err != nil {
		t.Fatalf("project require: %v", err)
	}
	if err := r.ExecuteChunk(t.Context(), `assert(require("pmod").v == 3)`, "init.lua", OriginUser); err != nil {
		t.Fatalf("user require: %v", err)
	}
}

// TestLoadFileStepSkipsMissing: a missing file is skipped, and a present one
// runs.
func TestLoadFileStepSkipsMissing(t *testing.T) {
	r, cfg, _ := newRequireRuntime(t)
	path := filepath.Join(cfg, "init.lua")
	writeFile(t, path, "LOADED = true\n")
	for _, p := range []string{filepath.Join(cfg, "missing.lua"), path} {
		if err := LoadFileStep("init", p, OriginUser).Run(t.Context(), r, r.current()); err != nil {
			t.Errorf("LoadFileStep(%s) = %v", filepath.Base(p), err)
		}
	}
}

func TestClassifyMostSpecific(t *testing.T) {
	pc := PathClasses{
		{Prefix: "/cfg", Origin: OriginUser},
		{Prefix: "/cfg/repo/.agents", Origin: OriginProject},
	}
	for path, want := range map[string]Origin{
		"/cfg/init.lua":              OriginUser,
		"/cfg/repo/.agents/init.lua": OriginProject,
		"/cfgx/init.lua":             "",
	} {
		if got := pc.Classify(path); got != want {
			t.Errorf("Classify(%s) = %q, want %q", path, got, want)
		}
	}
}

// TestClassifyPrefixCreatedLater: a class registered through a symlink
// before its directory exists (data/skills on first run) still matches
// once the directory is made, though it was resolved before.
func TestClassifyPrefixCreatedLater(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	pc := PathClasses{{Prefix: filepath.Join(root, "link", "skills"), Origin: OriginProject}}
	file := filepath.Join(root, "skills", "a.lua")
	pc.Classify(file)
	writeFile(t, file, "")
	if got := pc.Classify(file); got != OriginProject {
		t.Fatalf("after mkdir: %q, want %q", got, OriginProject)
	}
}

// TestOriginFollowsCode: the origin a call runs as is the innermost Lua file
// on its stack, so a plugin's setup() that user init.lua calls runs as the
// plugin and a user callback it calls runs as user, a module runs under its
// own file's class, and a forged loadstring name launders nothing.
func TestOriginFollowsCode(t *testing.T) {
	r, cfg, plugin := newRequireRuntime(t)
	dir := filepath.Join(plugin, "local", "omod")
	writeFile(t, filepath.Join(dir, pluginpkg.CodeDir, "lua", "omod.lua"), `_origin()
		return { setup = function(f) _origin() f()
			local _, forged = pcall(loadstring, "_origin()", "@user:/x") forged() end }`)
	r.rfs = fstest.MapFS{"lua/zemi/util.lua": {Data: []byte(`_origin() return { call = function(f) f() end }`)}}
	boot := BootStep{Name: "plugins", Run: func(_ context.Context, _ *Runtime, g *Generation) error {
		g.Plugins = []string{dir}
		return nil
	}}
	if err := r.Boot(t.Context(), []BootStep{boot}); err != nil {
		t.Fatal(err)
	}
	var seen []string
	probeOrigin(t, r, &seen)
	if err := r.ExecuteChunk(t.Context(), `
		require("omod").setup(function() _origin() end)
		require("zemi.util").call(function() _origin() end)
	`, filepath.Join(cfg, "init.lua"), OriginUser); err != nil {
		t.Fatal(err)
	}
	want := "plugin plugin user plugin builtin user"
	if got := strings.Join(seen, " "); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// probeOrigin makes _origin() record the origin of the code running at the
// call, for the tests that pin what code runs as.
func probeOrigin(t *testing.T, r *Runtime, seen *[]string) {
	t.Helper()
	g := r.current()
	g.SB.frozen.RawSetString("_origin", g.L.NewFunction(func(L *glua.LState) int {
		*seen = append(*seen, string(r.CurrentOrigin(L)))
		return 0
	}))
	g.SB.protected.Add("_origin")
}
