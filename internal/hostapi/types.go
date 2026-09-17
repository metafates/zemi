// types.go writes the shipped LuaLS annotations for the zemi.* API to the
// data dir and points a lua-language-server at them, so init.lua gets
// completion and type checking the way vim.* does in Neovim. The file is
// a description of the API, never part of it: nothing requires it and the
// interpreter never sees it.
package hostapi

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	zemiruntime "github.com/metafates/zemi/runtime"
)

// TypesDir is where the annotations land: the data dir, not the config
// repo, so a generated file never shows up in the user's config diff.
func (h *Host) TypesDir() string { return joinIf(h.Dirs.DataHome, "types") }

// syncTypes rewrites the annotations and the .luarc.json beside init.lua
// on every start, so an upgraded zemi replaces both. Failures are logged:
// an editor convenience must never stop a boot.
func (h *Host) syncTypes() {
	dir := h.TypesDir()
	if dir == "" {
		return
	}
	if err := h.writeTypes(dir); err != nil {
		h.Log.Warn("types", "annotations not written", map[string]any{"error": err.Error()})
		return
	}
	if err := h.writeLuarc(dir); err != nil {
		h.Log.Warn("types", "luarc not written", map[string]any{"error": err.Error()})
	}
}

// writeTypes copies the embedded annotations plus the runtime modules
// config can require, so require("zemi.util") resolves to real source.
func (h *Host) writeTypes(dir string) error {
	trees := []struct {
		fsys fs.FS
		root string
		// strip is dropped from each path, the rest hangs under dir
		strip string
	}{
		{zemiruntime.Types, "types", "types/"},
		{zemiruntime.FS, "lua", "lua/"},
	}
	// a module zemi no longer ships must not linger as a reference
	if err := os.RemoveAll(filepath.Join(dir, "zemi")); err != nil {
		return err
	}
	for _, t := range trees {
		err := fs.WalkDir(t.fsys, t.root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := fs.ReadFile(t.fsys, p)
			if err != nil {
				return err
			}
			out := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(p, t.strip)))
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			return os.WriteFile(out, data, 0o644)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// writeLuarc writes the lua-language-server settings zemi generates,
// replacing an edited file: the settings describe zemi's sandbox, and a
// stale copy costs the user diagnostics on every line. It writes only when
// the content moved, since the config watcher reloads on an mtime.
func (h *Host) writeLuarc(typesDir string) error {
	if h.Dirs.ConfigHome == "" {
		return nil
	}
	path := filepath.Join(h.Dirs.ConfigHome, ".luarc.json")
	data, err := json.MarshalIndent(map[string]any{
		"$schema": "https://raw.githubusercontent.com/LuaLS/vscode-lua/master/setting/schema.json",
		// the sandbox's own basic, os and debug are described in zemi.lua
		"runtime": map[string]any{"version": "Lua 5.1", "builtin": map[string]string{
			"basic": "disable", "io": "disable", "os": "disable", "debug": "disable", "utf8": "disable",
		}},
		"workspace": map[string]any{"library": []string{typesDir}, "checkThirdParty": false},
		"telemetry": map[string]any{"enable": false},
	}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	return os.WriteFile(path, data, 0o644)
}
