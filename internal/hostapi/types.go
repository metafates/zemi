// types.go writes the shipped LuaLS annotations for the zemi.* API to the
// data dir and points a lua-language-server at them, so init.lua gets
// completion and type checking the way vim.* does in Neovim. The file is
// a description of the API, never part of it: nothing requires it and the
// interpreter never sees it.

package hostapi

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/set"
	zemiruntime "github.com/metafates/zemi/runtime"
)

// TypesDir is where the annotations land: the data dir, not the config
// repo, so a generated file never shows up in the user's config diff.
func (h *Host) TypesDir() string { return joinIf(h.Dirs.DataHome, "types") }

// syncTypes rewrites the annotations and the editor configs beside init.lua
// and in each local plugin on every start, so an upgraded zemi replaces
// them. Failures are logged: an editor convenience must never stop a boot.
func (h *Host) syncTypes() {
	dir := h.TypesDir()
	if dir == "" {
		return
	}
	if err := h.writeTypes(dir); err != nil {
		h.Log.Warn("annotations not written", "origin", "types", "error", err)
		return
	}
	trees := []string{h.Dirs.ConfigHome}
	ps, err := plugin.Scan(h.Dirs)
	if err != nil {
		h.Log.Warn("plugins not scanned for editor configs", "origin", "types", "error", err)
	}
	for _, p := range ps {
		// a clone is git's, and zemi does not load a symlinked dir
		if fi, err := os.Lstat(p.Dir); p.Local && err == nil && fi.IsDir() {
			trees = append(trees, p.Dir)
		}
	}
	for _, d := range trees {
		if err := writeEditorConfigs(d, dir); err != nil {
			h.Log.Warn("editor config not written", "origin", "types", "dir", d, "error", err)
		}
	}
}

// writeTypes copies the embedded annotations plus the runtime modules
// config can require, so require("zemi.util") resolves to real source.
func (h *Host) writeTypes(dir string) error {
	types, err := fs.Sub(zemiruntime.Types, "types")
	if err != nil {
		return err
	}
	mods, err := fs.Sub(zemiruntime.FS, "lua")
	if err != nil {
		return err
	}
	return replaceDir(dir, types, mods)
}

// replaceDir fills a temp sibling of dir from trees, renames each file over
// its old path and then removes what the new tree lacks, so another zemi
// window reading dir never misses a file both versions ship.
func replaceDir(dir string, trees ...fs.FS) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, t := range trees {
		if err := os.CopyFS(tmp, t); err != nil {
			return err
		}
	}
	keep := set.Set[string]{}
	err = filepath.WalkDir(tmp, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(tmp, p)
		keep.Add(rel)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dir, rel), 0o755)
		}
		return os.Rename(p, filepath.Join(dir, rel))
	})
	if err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		// another window may have removed it first
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if keep.Has(rel) {
			return nil
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
}

// writeEditorConfigs writes the settings zemi generates for the two editors
// that read the annotations, each in its own dialect, replacing an edited
// file: the settings describe the API, and a stale copy costs the user
// diagnostics on every line.
func writeEditorConfigs(dir, typesDir string) error {
	if dir == "" {
		return nil
	}
	for _, f := range []struct {
		name string
		doc  map[string]any
	}{{".luarc.json", luarcSettings(typesDir)}, {".emmyrc.json", emmyrcSettings(typesDir)}} {
		if err := writeConfig(filepath.Join(dir, f.name), f.doc); err != nil {
			return err
		}
	}
	return nil
}

// luarcSettings is lua-language-server's dialect, in the file it reads.
func luarcSettings(typesDir string) map[string]any {
	return map[string]any{
		"$schema": "https://raw.githubusercontent.com/LuaLS/vscode-lua/master/setting/schema.json",
		// the interpreter.s own basic, os and debug are described in zemi.lua
		"runtime": map[string]any{"version": "Lua 5.1", "builtin": map[string]string{
			"basic": "disable", "io": "disable", "os": "disable", "debug": "disable", "utf8": "disable",
		}},
		"workspace": map[string]any{"library": []string{typesDir}, "checkThirdParty": false},
		// near-certain bugs, raised to errors since those are what reach the
		// agent after a write; the rest misfire on sound code (a pcall result)
		"diagnostics": map[string]any{"severity": map[string]string{
			"undefined-global": "Error!", "missing-fields": "Error!", "assign-type-mismatch": "Error!",
			"missing-parameter": "Error!", "redundant-parameter": "Error!",
		}},
		"telemetry": map[string]any{"enable": false},
	}
}

// emmyrcSettings is EmmyLua's dialect, in the file of its own that it reads
// first: it takes a lua-language-server severity for an unknown value and then
// drops the whole config, this library included, leaving zemi.* undefined.
func emmyrcSettings(typesDir string) map[string]any {
	return map[string]any{
		"$schema":   "https://raw.githubusercontent.com/EmmyLuaLs/emmylua-analyzer-rust/refs/heads/main/crates/emmylua_code_analysis/resources/schema.json",
		"runtime":   map[string]any{"version": "Lua5.1"},
		"workspace": map[string]any{"library": []string{typesDir}},
		"diagnostics": map[string]any{"severity": map[string]string{
			"undefined-global": "error", "missing-fields": "error", "assign-type-mismatch": "error",
			"missing-parameter": "error", "redundant-parameter": "error",
		}},
	}
}

// writeConfig writes doc, indented and newline-terminated, only when the
// content moved: an open editor reloads its workspace on each write.
func writeConfig(path string, doc map[string]any) error {
	data, err := json.Marshal(doc, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// a plugin repo may ship the file as a link; WriteFile would clobber its target
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	return os.WriteFile(path, data, 0o644)
}
