package lua

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
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
	if err := r.Boot(context.Background(), nil); err != nil {
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

	if err := r.ExecuteChunk(context.Background(), `
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

	if err := r.ExecuteChunk(context.Background(), `
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

	if err := r.ExecuteChunk(context.Background(), `
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

	if err := r.ExecuteChunk(context.Background(), `
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

// TestRequireProjectModules: .zemi/lua is on the path for project code only.
func TestRequireProjectModules(t *testing.T) {
	proj, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := NewRuntime(RuntimeOptions{PathClass: PathClasses{{Prefix: proj, Origin: OriginProject}}, RuntimeFS: fstest.MapFS{}})
	if err := r.Boot(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(proj, "lua", "pmod.lua"), `return { v = 3 }`)
	if err := r.ExecuteChunk(context.Background(), `assert(require("pmod").v == 3)`, filepath.Join(proj, "init.lua"), OriginProject); err != nil {
		t.Fatalf("project require: %v", err)
	}
	if err := r.ExecuteChunk(context.Background(), `assert(not pcall(require, "pmod"), "user code loaded a project module")`, "init.lua", OriginUser); err != nil {
		t.Fatalf("user require: %v", err)
	}
}

// TestLoadFileStepOrigin: a missing file is skipped, and a file whose path
// classifies to another origin than the step declares is refused rather
// than run under the path's origin.
func TestLoadFileStepOrigin(t *testing.T) {
	r, cfg, _ := newRequireRuntime(t)
	path := filepath.Join(cfg, "init.lua")
	writeFile(t, path, "LOADED = true\n")
	for _, c := range []struct {
		path   string
		origin Origin
		ok     bool
	}{
		{filepath.Join(cfg, "missing.lua"), OriginUser, true},
		{path, OriginUser, true},
		{path, OriginProject, false},
	} {
		err := LoadFileStep("init", c.path, c.origin).Run(context.Background(), r, r.current())
		if (err == nil) != c.ok {
			t.Errorf("LoadFileStep(%s as %s) = %v", filepath.Base(c.path), c.origin, err)
		}
	}
}

func TestClassifyMostSpecific(t *testing.T) {
	pc := PathClasses{
		{Prefix: "/cfg", Origin: OriginUser},
		{Prefix: "/cfg/repo/.zemi", Origin: OriginProject},
	}
	for path, want := range map[string]Origin{
		"/cfg/init.lua":            OriginUser,
		"/cfg/repo/.zemi/init.lua": OriginProject,
		"/cfgx/init.lua":           "",
	} {
		if got := pc.Classify(path); got != want {
			t.Errorf("Classify(%s) = %q, want %q", path, got, want)
		}
	}
}

// TestClassifyPrefixCreatedLater: a class registered through a symlink
// before its directory exists (data/skills on first run) still matches
// once the directory is made.
func TestClassifyPrefixCreatedLater(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	pc := PathClasses{{Prefix: filepath.Join(root, "link", "skills"), Origin: OriginLearned}}
	file := filepath.Join(root, "skills", "a.lua")
	if got := pc.Classify(file); got != "" {
		t.Fatalf("before mkdir: %q", got)
	}
	writeFile(t, file, "")
	if got := pc.Classify(file); got != OriginLearned {
		t.Fatalf("after mkdir: %q, want %q", got, OriginLearned)
	}
}

// TestRequireOriginFollowsFile verifies §5: origin comes from the module
// file's directory class, never from the requirer, so a plugin module
// required from user code reports plugin origin (and the user chunk itself
// reports user).
func TestRequireOriginFollowsFile(t *testing.T) {
	r, _, plugin := newRequireRuntime(t)
	writeFile(t, filepath.Join(plugin, "p", "start", "omod", "lua", "omod.lua"),
		"print(\"hello from plugin\")\n")

	var mu sync.Mutex
	var lines []string
	r.SetPrintHook(func(origin, line string) {
		mu.Lock()
		lines = append(lines, origin+"|"+line)
		mu.Unlock()
	})

	if err := r.ExecuteChunk(context.Background(), `
		require("omod")
		print("after require")
	`, "init.lua", OriginUser); err != nil {
		t.Fatalf("chunk failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("expected 2 print lines, got %v", lines)
	}
	if !strings.Contains(lines[0], "plugin|") || !strings.Contains(lines[0], "hello from plugin") {
		t.Fatalf("plugin module ran under wrong origin: %q", lines[0])
	}
	if !strings.Contains(lines[1], "user|") || !strings.Contains(lines[1], "after require") {
		t.Fatalf("user chunk ran under wrong origin: %q", lines[1])
	}
}

// TestRequireInstancePerOrigin: code below a module's origin gets its own
// instance run under its own origin, so functions it plants (in a table, a
// callback list, a nested module or zemi.util) never reach user code; a
// forged loadstring name neither.
func TestRequireInstancePerOrigin(t *testing.T) {
	dirs := map[Origin]string{}
	var pc PathClasses
	for _, o := range []Origin{OriginUser, OriginPlugin, OriginProject, OriginLearned} {
		d, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		dirs[o] = d
		pc = append(pc, PathClass{Prefix: d, Origin: o})
	}
	r := NewRuntime(RuntimeOptions{PathClass: pc, RuntimeFS: fstest.MapFS{
		"lua/zemi/util.lua": {Data: []byte(`return { nested = {} }`)},
	}})
	if err := r.Boot(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	var lines []string
	r.SetPrintHook(func(origin, line string) { lines = append(lines, origin+"|"+line) })
	cfg := dirs[OriginUser]
	writeFile(t, filepath.Join(cfg, "lua", "myutil.lua"), `print("body") return { go = function() end, util = require("./util") }`)
	writeFile(t, filepath.Join(cfg, "lua", "util.lua"), `return {}`)
	writeFile(t, filepath.Join(cfg, "lua", "hub.lua"), `local fns = {} return { on = function(f) fns[#fns + 1] = f end, fire = function() for _, f in ipairs(fns) do f() end end }`)
	run := func(src string, o Origin) {
		t.Helper()
		if err := r.ExecuteChunk(context.Background(), src, filepath.Join(dirs[o], "init.lua"), o); err != nil {
			t.Fatalf("%s chunk: %v", o, err)
		}
	}
	run(`require("myutil")`, OriginUser)
	for _, o := range []Origin{OriginPlugin, OriginProject, OriginLearned} {
		run(`
			local planted = function() print("planted") end
			local u = require("myutil")
			u.go, u.util.go = planted, planted
			assert(not pcall(function() require("zemi.util").go = planted end), "builtin module writable")
			assert(not pcall(function() require("zemi.util").nested.go = planted end), "nested builtin table writable")
			require("hub").on(planted)
			local _, f = pcall(loadstring, "error('boom')", "@user:/x")
			local _, err = pcall(f)
			assert(err:find("@`+string(o)+`:", 1, true) == 1, "forged chunk name kept: " .. err)
			local _, g = pcall(loadstring, "return require('./lua/myutil')", "`+filepath.ToSlash(filepath.Join(cfg, "init.lua"))+`")
			assert(not pcall(g), "forged chunk name resolved a relative require in the user tree")
		`, o)
	}
	run(`
		local u = require("myutil")
		u.go()
		assert(u.util.go == nil, "nested module instance shared")
		assert(require("zemi.util").go == nil and require("zemi.util").nested.go == nil, "zemi.util written through")
		require("hub").fire()
	`, OriginUser)
	got := strings.Join(lines, "\n")
	if strings.Contains(got, "planted") {
		t.Fatalf("lower origin code ran from a user call:\n%s", got)
	}
	if !strings.Contains(got, "project|body") || strings.Count(got, "user|body") != 1 {
		t.Fatalf("module bodies ran under the wrong origins:\n%s", got)
	}
}
