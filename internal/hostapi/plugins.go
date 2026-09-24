package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/plugins"
)

// pluginLoad is one enabled plugin as the last boot saw it; err is nil when
// its init.lua ran.
type pluginLoad struct {
	name, dir string
	err       error
}

// PluginsStep loads every enabled plugin's init.lua as plugin origin, sorted
// by name (DESIGN §5), after fixing the generation's plugin roots, so one
// plugin may require another's modules. A failing plugin does not stop the
// rest. It runs no git.
func (h *Host) PluginsStep() lua.BootStep {
	return lua.BootStep{
		Name: "plugins",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			ps, err := plugins.Scan(h.Dirs)
			if err == nil {
				// a failed scan lists no plugins, and the sweep would take every
				// plugin's saved tokens with it
				h.sweepPluginState(ps)
			}
			installed := map[string]bool{}
			for _, p := range ps {
				if !p.Local {
					installed[p.Name] = true
				}
			}
			slices.SortStableFunc(ps, func(a, b plugins.Plugin) int { return cmp.Compare(a.Name, b.Name) })
			var loads []pluginLoad
			var run []plugins.Plugin
			for _, p := range ps {
				fi, lerr := os.Lstat(p.Dir)
				switch {
				case !p.Enabled:
					continue
				case p.Local && installed[p.Name]:
					loads = append(loads, pluginLoad{p.Name, p.Dir, errors.New("an installed plugin has this name; delete it or rename this one")})
				case lerr == nil && !fi.IsDir():
					// origin is classified on resolved paths, so its files would
					// be nobody's
					loads = append(loads, pluginLoad{p.Name, p.Dir, errors.New("a symlink, which zemi does not load; move the plugin into " + filepath.Dir(p.Dir))})
				default:
					g.Plugins = append(g.Plugins, p.Dir)
					run = append(run, p)
				}
			}
			for _, p := range run {
				init := filepath.Join(p.Dir, "init.lua")
				loads = append(loads, pluginLoad{p.Name, p.Dir, lua.LoadFileStep("plugin "+p.Name, init, lua.OriginPlugin).Run(ctx, r, g)})
			}
			slices.SortStableFunc(loads, func(a, b pluginLoad) int { return cmp.Compare(a.name, b.name) })
			h.pluginLoads.Store(&loads)
			errs := []error{err}
			for _, l := range loads {
				if l.err != nil {
					errs = append(errs, fmt.Errorf("plugin %s: %w", l.name, l.err))
				}
			}
			return errors.Join(errs...)
		},
	}
}

// sweepPluginState drops the zemi.state keys of plugins whose directory is
// gone, so a deleted plugin's saved tokens go with it.
func (h *Host) sweepPluginState(ps []plugins.Plugin) {
	if h.GlobalDB == nil {
		return
	}
	keys, _ := h.GlobalDB.KeysKV("plugin:")
	for _, k := range keys {
		name, _, _ := strings.Cut(strings.TrimPrefix(k, "plugin:"), ":")
		if !slices.ContainsFunc(ps, func(p plugins.Plugin) bool { return p.Name == name }) {
			_, _ = h.GlobalDB.DelKV(k)
		}
	}
}

// localPlugins holds the plugins the user develops in place. The model's
// file tools read it without asking; it is no read jail, which would let
// every plugin's and a trusted project's zemi.fs.read reach them all.
func (h *Host) localPlugins() string {
	return filepath.Join(h.Dirs.PluginsRoot(), "local")
}

// PluginOf names the plugin a file belongs to, its directory under
// installed/ or local/, or "" for any other path.
func (h *Host) PluginOf(path string) string {
	sep := string(filepath.Separator)
	rel, ok := strings.CutPrefix(path, h.pluginsRoot()+sep)
	kind, rest, _ := strings.Cut(rel, sep)
	name, _, _ := strings.Cut(rest, sep)
	if !ok || kind != "local" && kind != "installed" {
		return ""
	}
	return name
}

// pluginClash refuses one plugin's registration over another's: both rank
// as plugin, so the later would silently replace the first. owner is
// Host.PluginOf; a bare registry (tests) has none and refuses nothing.
func pluginClash(owner func(string) string, what string, old lua.Origin, oldPath string, new lua.Origin, newPath string) error {
	if owner == nil || old != lua.OriginPlugin || new != lua.OriginPlugin {
		return nil
	}
	if a := owner(oldPath); a != owner(newPath) {
		return fmt.Errorf("%s is already registered by plugin %s, not %s", what, a, owner(newPath))
	}
	return nil
}
