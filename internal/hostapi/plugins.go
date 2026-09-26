package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/plugins"
	"github.com/metafates/zemi/internal/skills"
	"github.com/metafates/zemi/internal/store"
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
			run, loads := pickPlugins(ps)
			roots := make([]string, len(run))
			for i, p := range run {
				kind := "installed"
				if p.Local {
					kind = "local"
				}
				roots[i] = filepath.Join(h.pluginsRoot(), kind, p.Name)
			}
			g.Plugins = roots
			h.pluginRoots.Store(&roots)
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

// pickPlugins sorts the enabled plugins by name and splits them into those
// the loader runs and those it refuses, with why.
func pickPlugins(ps []plugins.Plugin) (run []plugins.Plugin, refused []pluginLoad) {
	installed := map[string]bool{}
	for _, p := range ps {
		if !p.Local {
			installed[p.Name] = true
		}
	}
	slices.SortStableFunc(ps, func(a, b plugins.Plugin) int { return cmp.Compare(a.Name, b.Name) })
	for _, p := range ps {
		fi, err := os.Lstat(p.Dir)
		switch {
		case !p.Enabled:
		case p.Local && installed[p.Name]:
			refused = append(refused, pluginLoad{p.Name, p.Dir, errors.New("an installed plugin has this name; delete it or rename this one")})
		case err == nil && !fi.IsDir():
			// origin is classified on resolved paths, so its files would be
			// nobody's
			refused = append(refused, pluginLoad{p.Name, p.Dir, errors.New("a symlink, which zemi does not load; move the plugin into " + filepath.Dir(p.Dir))})
		default:
			run = append(run, p)
		}
	}
	return run, refused
}

// pluginDirs is the plugin roots the last boot fixed for its generation,
// resolved as PluginOf reads them.
func (h *Host) pluginDirs() []string {
	if p := h.pluginRoots.Load(); p != nil {
		return *p
	}
	return nil
}

// sweepPluginState drops the zemi.state keys of a plugin whose directory is
// gone or holds another install than the one that wrote them, so a deleted
// plugin's saved tokens never reach a new one of the same name, even one
// installed before this boot. The key "plugin:<name>", which no plugin can
// write, holds the install's id as a JSON string, since kv holds JSON that
// a user zemi.state read decodes.
func (h *Host) sweepPluginState(ps []plugins.Plugin) {
	if h.GlobalDB == nil {
		return
	}
	ids, fresh := map[string]string{}, map[string]bool{}
	for _, p := range ps {
		id := strconv.Quote(plugins.InstallID(p.Dir))
		was, err := h.GlobalDB.GetKV("plugin:" + p.Name)
		if errors.Is(err, store.ErrNotFound) {
			was = `""` // no id yet: a local plugin, or a clone made before ids
		} else if err != nil {
			was = id // a failed read must not drop the plugin's keys
		}
		ids[p.Name], fresh[p.Name] = id, was != id
	}
	keys, _ := h.GlobalDB.KeysKV("plugin:")
	for _, k := range keys {
		name, _, _ := strings.Cut(strings.TrimPrefix(k, "plugin:"), ":")
		if _, ok := ids[name]; !ok || fresh[name] {
			_, _ = h.GlobalDB.DelKV(k)
		}
	}
	for name, id := range ids {
		if fresh[name] && id != `""` {
			_ = h.GlobalDB.SetKV("plugin:"+name, id)
		}
	}
}

// localPlugins holds the plugins the user develops in place: auto mode and
// plain commands read it, and writes to it ask unless its creation was
// allowed (madePlugin). It is no read jail, which would let every plugin's
// and a trusted project's zemi.fs.read reach them all.
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

// localPluginDir is the local/<name> directory holding path, or "".
func (h *Host) localPluginDir(path string) string {
	name := h.PluginOf(path)
	if dir := filepath.Join(h.pluginsRoot(), "local", name); name != "" && strings.HasPrefix(path, dir) {
		return dir
	}
	return ""
}

// newPlugin is the local plugin directory a write to path would create, or
// "" when path is in none or it exists.
func (h *Host) newPlugin(path string) string {
	if dir := h.localPluginDir(path); dir != "" {
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			return dir
		}
	}
	return ""
}

// madePlugin says whether path is in a local plugin whose creation was
// allowed in this process.
func (h *Host) madePlugin(path string) bool {
	_, ok := h.madePlugins.Load(h.localPluginDir(path))
	return ok
}

// layerFile is one <name><ext> file of a layered resource directory.
type layerFile struct {
	name, path string
	origin     lua.Origin
}

// layerFiles lists the <name><ext> files in sub of the user config, each
// enabled plugin, then a trusted project's .agents. The first file of a
// name wins; /errors records one no skills.ValidName and one a plugin ships
// under a name another plugin took first.
func (h *Host) layerFiles(sub, ext string) []layerFile {
	var dirs []layerFile
	if h.Dirs.ConfigHome != "" {
		dirs = append(dirs, layerFile{path: filepath.Join(h.Dirs.ConfigHome, sub), origin: lua.OriginUser})
	}
	for _, p := range h.pluginDirs() {
		dirs = append(dirs, layerFile{path: filepath.Join(p, sub), origin: lua.OriginPlugin})
	}
	if pdir := h.Dirs.ProjectConfigDir(); pdir != "" && h.ProjectTrusted() {
		dirs = append(dirs, layerFile{path: filepath.Join(pdir, sub), origin: lua.OriginProject})
	}
	var out []layerFile
	seen := map[string]string{} // name -> the kept file
	for _, d := range dirs {
		entries, _ := os.ReadDir(d.path)
		for _, e := range entries {
			name, ok := strings.CutSuffix(e.Name(), ext)
			if !ok || e.IsDir() {
				continue
			}
			path := filepath.Join(d.path, e.Name())
			if kept, ok := seen[name]; ok {
				if a, b := h.PluginOf(kept), h.PluginOf(path); a != "" && b != "" && a != b {
					h.RecordErrorOnce("plugins", fmt.Sprintf("%s %s is already provided by plugin %s, not %s", strings.TrimSuffix(sub, "s"), name, a, b))
				}
				continue
			}
			if !skills.ValidName(name) {
				h.RecordErrorOnce(sub, path+": name must match [a-z0-9][a-z0-9-]*")
				continue
			}
			seen[name] = path
			out = append(out, layerFile{name, path, d.origin})
		}
	}
	return out
}
