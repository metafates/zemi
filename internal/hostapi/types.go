// types.go writes the shipped LuaLS annotations for the zemi.* API to the
// data dir and points a lua-language-server at them, so init.lua gets
// completion and type checking the way vim.* does in Neovim. The file is
// a description of the API, never part of it: nothing requires it and the
// interpreter never sees it.

package hostapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/metafates/zemi/internal/plugin"
	zemiruntime "github.com/metafates/zemi/runtime"
)

// TypesDir is where the annotations land: the data dir, not the config
// repo, so a generated file never shows up in the user's config diff.
func (h *Host) TypesDir() string { return joinIf(h.Dirs.DataHome, "types") }

// syncTypes rewrites the annotations and the .luarc.json beside init.lua
// and in each local plugin on every start, so an upgraded zemi replaces
// them. Failures are logged: an editor convenience must never stop a boot.
func (h *Host) syncTypes() {
	dir := h.TypesDir()
	if dir == "" {
		return
	}
	if err := h.writeTypes(dir); err != nil {
		h.Log.Warn("types", "annotations not written", map[string]any{"error": err.Error()})
		return
	}
	luarcs := []string{h.Dirs.ConfigHome}
	ps, err := plugin.Scan(h.Dirs)
	if err != nil {
		h.Log.Warn("types", "plugins not scanned for luarc", map[string]any{"error": err.Error()})
	}
	for _, p := range ps {
		// a clone is git's, and zemi does not load a symlinked dir
		if fi, err := os.Lstat(p.Dir); p.Local && err == nil && fi.IsDir() {
			luarcs = append(luarcs, p.Dir)
		}
	}
	for _, d := range luarcs {
		if err := writeLuarc(d, dir); err != nil {
			h.Log.Warn("types", "luarc not written", map[string]any{"dir": d, "error": err.Error()})
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
	keep := map[string]bool{}
	err = filepath.WalkDir(tmp, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(tmp, p)
		keep[rel] = true
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
		if keep[rel] {
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

// writeLuarc writes the lua-language-server settings zemi generates,
// replacing an edited file: the settings describe zemi's sandbox, and a
// stale copy costs the user diagnostics on every line. It writes only when
// the content moved: an open editor reloads its workspace on each write.
func writeLuarc(dir, typesDir string) error {
	if dir == "" {
		return nil
	}
	path := filepath.Join(dir, ".luarc.json")
	data, err := json.MarshalIndent(map[string]any{
		"$schema": "https://raw.githubusercontent.com/LuaLS/vscode-lua/master/setting/schema.json",
		// the sandbox's own basic, os and debug are described in zemi.lua
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
	}, "", "  ")
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
