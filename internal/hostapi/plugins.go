package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/skill"
	"github.com/metafates/zemi/internal/store"
)

// pluginLoad is one enabled plugin as the last boot saw it; err is nil when
// its init.lua ran.
type pluginLoad struct {
	name, dir string
	err       error
}

// pluginGrant is what the plugins step fixed for one generation: the roots it
// loaded, the capabilities each manifest declared, and which roots are
// Claude-layout (their layered names are prefixed and their MCP servers and
// hooks load). One swap, so a reload, a disable or a delete cannot leave a
// stale grant.
type pluginGrant struct {
	roots  []string
	uses   map[string][]string // by plugin name
	claude map[string]bool     // root -> it is a Claude-layout plugin
}

// PluginsStep loads every enabled plugin's init.lua as plugin origin, sorted
// by name (DESIGN §5), after fixing the generation's plugin roots, so one
// plugin may require another's modules. A failing plugin does not stop the
// rest. It runs no git.
func (h *Host) PluginsStep() lua.BootStep {
	return lua.BootStep{
		Name: "plugins",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			ps, err := plugin.Scan(h.Dirs)
			if err == nil {
				// a failed scan lists no plugins, and the sweep would take every
				// plugin's saved tokens with it
				h.sweepPluginState(ps)
			}
			run, loads := pickPlugins(ps)
			grant := pluginGrant{roots: make([]string, len(run)), uses: map[string][]string{}, claude: map[string]bool{}}
			hooks := map[string][]hookEntry{}
			for i, p := range run {
				kind := "installed"
				if p.Local {
					kind = "local"
				}
				root := filepath.Join(h.pluginsRoot(), kind, p.Name)
				grant.roots[i] = root
				grant.uses[p.Name] = p.Uses
				if !plugin.IsClaude(root) {
					continue
				}
				grant.claude[root] = true
				data := h.pluginDataDir(p.Name)
				if mkErr := os.MkdirAll(data, 0o755); mkErr != nil {
					h.Log.Warn("plugin data dir not created", "origin", "plugins", "plugin", p.Name, "error", mkErr)
				}
				m, rerr := plugin.ReadClaude(root, data)
				if rerr != nil {
					h.Log.Warn("claude manifest unread, its servers and hooks skipped", "origin", "plugins", "plugin", p.Name, "error", rerr)
					continue
				}
				h.addClaudeMCP(p.Name, root, data, m.MCP)
				for event, list := range h.buildHooks(p.Name, root, data, m.Hooks) {
					hooks[event] = append(hooks[event], list...)
				}
			}
			g.Plugins = grant.roots
			h.pluginGrant.Store(&grant)
			h.hookReg.set(Gen(g.ID), hooks)
			for _, p := range run {
				init := filepath.Join(p.Dir, plugin.InitFile)
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
func pickPlugins(ps []plugin.Plugin) (run []plugin.Plugin, refused []pluginLoad) {
	installed := map[string]bool{}
	for _, p := range ps {
		if !p.Local {
			installed[p.Name] = true
		}
	}
	slices.SortStableFunc(ps, func(a, b plugin.Plugin) int { return cmp.Compare(a.Name, b.Name) })
	for _, p := range ps {
		fi, err := os.Lstat(p.Dir)
		uses := plugin.CheckUses(p.Uses)
		// a link reads as its target, so its layout is not this plugin's to check
		var old error
		if err == nil && fi.IsDir() {
			old = plugin.OldLayout(p.Dir)
		}
		switch {
		case !p.Enabled:
		case uses != nil:
			refused = append(refused, pluginLoad{p.Name, p.Dir, uses})
		case p.Local && installed[p.Name]:
			refused = append(refused, pluginLoad{p.Name, p.Dir, errors.New("an installed plugin has this name; delete it or rename this one")})
		case err == nil && !fi.IsDir():
			// origin is classified on resolved paths, so its files would be
			// nobody's
			refused = append(refused, pluginLoad{p.Name, p.Dir, errors.New("a symlink, which zemi does not load; move the plugin into " + filepath.Dir(p.Dir))})
		case old != nil:
			refused = append(refused, pluginLoad{p.Name, p.Dir, old})
		default:
			run = append(run, p)
		}
	}
	return run, refused
}

// PluginDir is the directory of the plugin named name that the plugins step
// would load, turned off or not, or why it would load none.
func PluginDir(d config.Dirs, name string) (string, error) {
	ps, err := plugin.Scan(d)
	if err != nil {
		return "", err
	}
	for i := range ps {
		ps[i].Enabled = true
	}
	run, refused := pickPlugins(ps)
	for _, p := range run {
		if p.Name == name {
			return p.Dir, nil
		}
	}
	for _, l := range refused {
		if l.name == name {
			return "", fmt.Errorf("%s: %w", l.dir, l.err)
		}
	}
	return "", fmt.Errorf("no plugin is named %q in %s", name, d.PluginsRoot())
}

// pluginDirs is the plugin roots the last boot fixed for its generation,
// resolved as PluginOf reads them.
func (h *Host) pluginDirs() []string {
	if p := h.pluginGrant.Load(); p != nil {
		return p.roots
	}
	return nil
}

// pluginDataDir is the data dir a Claude plugin's ${CLAUDE_PLUGIN_DATA}
// stands for, where its hooks and servers may keep state.
func (h *Host) pluginDataDir(name string) string {
	return filepath.Join(h.Dirs.PluginsRoot(), "data", name)
}

// isClaudePlugin reports whether root is a Claude-layout plugin's root.
func (h *Host) isClaudePlugin(root string) bool {
	g := h.pluginGrant.Load()
	return g != nil && g.claude[root]
}

// claudeRootOf is the Claude-layout plugin root path belongs to, or "".
func (h *Host) claudeRootOf(path string) string {
	sep := string(filepath.Separator)
	if g := h.pluginGrant.Load(); g != nil {
		for root := range g.claude {
			if strings.HasPrefix(path, root+sep) {
				return root
			}
		}
	}
	return ""
}

// pluginPrefix is the layered-name prefix of a Claude-layout plugin: its
// sanitized name and a dash, so two plugins cannot ship one name.
func (h *Host) pluginPrefix(root string) string {
	return plugin.SanitizeName(h.PluginOf(root)) + "-"
}

// layerPrefix is the name prefix a Claude-layout plugin's layered files
// load under; workflows stay flat, their names no user types.
func (h *Host) layerPrefix(root, sub string) string {
	if sub == "workflows" || !h.isClaudePlugin(root) {
		return ""
	}
	return h.pluginPrefix(root)
}

// claudeCommandSkills turns a Claude plugin's commands/*.md into
// model-invocable, not-user-invocable skill rows named <prefix><stem>:
// Claude loads commands and skills as one thing, zemi's /command stays
// mdcommands'. A name that would break skill.ValidName is skipped, warned
// by the command load.
func (h *Host) claudeCommandSkills(root, prefix, data string) []skill.Skill {
	entries, err := os.ReadDir(filepath.Join(root, "commands"))
	if err != nil {
		return nil
	}
	var rows []skill.Skill
	for _, e := range entries {
		stem, ok := strings.CutSuffix(e.Name(), ".md")
		if !ok || e.IsDir() || !skill.ValidName(prefix+stem) {
			continue
		}
		path := filepath.Join(root, "commands", e.Name())
		block, body, err := readFrontmatterFile(path)
		if err != nil {
			continue
		}
		var fm struct {
			Description string `yaml:"description"`
		}
		_ = yaml.Unmarshal([]byte(block), &fm)
		desc := strings.TrimSpace(fm.Description)
		if desc == "" {
			desc = skill.FirstBodyLine(body)
		}
		rows = append(rows, skill.Skill{
			Name: prefix + stem, Dir: filepath.Join(root, "commands"), Source: skill.Plugin,
			Description: learn.Truncate(desc, skill.MaxDescriptionRunes),
			File:        path, Root: root, Data: data,
		})
		if strings.Contains(body, "!`") {
			h.Log.Warn("command body has !`cmd` shell blocks, they stay literal text", "origin", "skills", "command", e.Name())
		}
	}
	return rows
}

// addClaudeMCP registers a Claude plugin's MCP servers under its prefix.
func (h *Host) addClaudeMCP(name, root, data string, servers []plugin.ClaudeMCPServer) {
	prefix := plugin.SanitizeName(name) + "-"
	for _, s := range servers {
		if s.Type == "sse" || s.Type == "ws" {
			h.Log.Warn("mcp server type not supported, skipped", "origin", "mcp", "plugin", name, "server", s.Name, "type", s.Type)
			continue
		}
		server := prefix + plugin.SanitizeName(s.Name)
		if !mcpServerName.MatchString(server) {
			h.Log.Warn("mcp server name does not fit zemi's rule, skipped", "origin", "mcp", "plugin", name, "server", s.Name)
			continue
		}
		spec := mcpSpec{Command: s.Command, Args: s.Args, URL: s.URL, Headers: s.Headers,
			ClientID: s.ClientID, ClientSecret: s.ClientSecret, CallbackPort: s.CallbackPort}
		if spec.Command != "" {
			env := maps.Clone(s.Env)
			if env == nil {
				env = map[string]string{}
			}
			// Claude injects these into stdio children even unreferenced
			env["CLAUDE_PLUGIN_ROOT"], env["CLAUDE_PLUGIN_DATA"] = root, data
			spec.Env = env
		}
		if err := h.addMCP(server, spec); err != nil {
			h.Log.Warn("mcp server not registered", "origin", "mcp", "plugin", name, "server", s.Name, "error", err)
		}
	}
}

// pluginDeclares reports whether the plugin named name declared the capability
// cap in the manifest of the generation that loaded it.
func (h *Host) pluginDeclares(name, cap string) bool {
	g := h.pluginGrant.Load()
	return g != nil && name != "" && slices.Contains(g.uses[name], cap)
}

// sweepPluginState drops the zemi.state keys and kept setting picks of a
// plugin whose directory is gone or holds another install than the one that
// wrote them, so a deleted plugin's saved tokens never reach a new one of
// the same name, even one installed before this boot. The "plugin:<name>" key
// holds the install's id as a JSON string, since kv holds JSON that a user
// zemi.state read decodes; no plugin can write that key.
func (h *Host) sweepPluginState(ps []plugin.Plugin) {
	if h.GlobalDB == nil {
		return
	}
	ids, fresh := map[string]string{}, map[string]bool{}
	for _, p := range ps {
		id := strconv.Quote(plugin.InstallID(p.Dir))
		was, err := h.GlobalDB.GetKV("plugin:" + p.Name)
		if errors.Is(err, store.ErrNotFound) {
			was = `""` // no id kept: a local plugin, or a clone no sweep has seen
		} else if err != nil {
			was = id // a failed read must not drop the plugin's keys
		}
		ids[p.Name], fresh[p.Name] = id, was != id
	}
	keys, _ := h.GlobalDB.KeysKV("plugin:")
	picks, _ := h.GlobalDB.KeysKV("config:plugins.")
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	for _, k := range append(keys, picks...) {
		// a name holds no "." or ":"; its state keys are plugin:<name>:<key>,
		// its kept settings config:plugins.<name>.<key>
		name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(k, "plugin:"), "config:plugins."), ":")
		name, _, _ = strings.Cut(name, ".")
		if _, ok := ids[name]; ok && !fresh[name] {
			continue
		}
		if path, ok := strings.CutPrefix(k, "config:"); ok {
			h.forgetPick(path)
		} else {
			_, _ = h.GlobalDB.DelKV(k)
		}
	}
	for name, id := range ids {
		if fresh[name] && id != `""` {
			_ = h.GlobalDB.SetKV("plugin:"+name, id)
		}
	}
}

// localPlugins holds the plugins the user develops in place: the prompt
// names it. It is no read jail, which would let every plugin's
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

// OriginBadge marks what plugins and project config draw or run
// ("plugin:jira", "project:init.lua"), "" for the user's and zemi's own: a
// plugin's name, else the file's, since a path leaves no room for a title.
func (h *Host) OriginBadge(o lua.Origin, path string) string {
	switch {
	case o == lua.OriginBuiltin || o == lua.OriginUser:
		return ""
	case h.PluginOf(path) != "":
		return string(o) + ":" + h.PluginOf(path)
	case path != "":
		return string(o) + ":" + filepath.Base(path)
	}
	return string(o)
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

// layerFile is one <name><ext> file of a layered resource directory.
type layerFile struct {
	name, path string
	origin     lua.Origin
	prefix     string // a Claude-layout plugin's layered-name prefix, "" for the rest
}

// pluginLayerDir is where a plugin keeps one layered resource dir: the Lua
// one (workflows) under CodeDir, the markdown ones at its root.
func pluginLayerDir(root, sub string) string {
	if sub == "workflows" {
		return filepath.Join(root, plugin.CodeDir, sub)
	}
	return filepath.Join(root, sub)
}

// layerFiles hands load the <name><ext> files in sub of the user config,
// each enabled plugin, then a trusted project's .agents. The first file of a
// name load accepts wins, so a broken one hides nothing; /errors records a
// name no skill.ValidName and one a plugin ships under a name another
// plugin took first.
func (h *Host) layerFiles(sub, ext string, load func(layerFile) bool) {
	var dirs []layerFile
	if h.Dirs.ConfigHome != "" {
		dirs = append(dirs, layerFile{path: filepath.Join(h.Dirs.ConfigHome, sub), origin: lua.OriginUser})
	}
	for _, p := range h.pluginDirs() {
		dirs = append(dirs, layerFile{path: pluginLayerDir(p, sub), origin: lua.OriginPlugin, prefix: h.layerPrefix(p, sub)})
	}
	if pdir := h.Dirs.ProjectConfigDir(); pdir != "" && h.ProjectTrusted() {
		dirs = append(dirs, layerFile{path: filepath.Join(pdir, sub), origin: lua.OriginProject})
	}
	seen := map[string]string{} // name -> the kept file
	for _, d := range dirs {
		entries, _ := os.ReadDir(d.path)
		for _, e := range entries {
			name, ok := strings.CutSuffix(e.Name(), ext)
			if !ok || e.IsDir() {
				continue
			}
			name = d.prefix + name
			path := filepath.Join(d.path, e.Name())
			if kept, ok := seen[name]; ok {
				if a, b := h.PluginOf(kept), h.PluginOf(path); a != "" && b != "" && a != b {
					h.RecordErrorOnce("plugins", fmt.Sprintf("%s %s is already provided by plugin %s, not %s", strings.TrimSuffix(sub, "s"), name, a, b))
				}
				continue
			}
			if !skill.ValidName(name) {
				h.RecordErrorOnce(sub, path+": name must be "+skill.NameRule)
				continue
			}
			if load(layerFile{name, path, d.origin, d.prefix}) {
				seen[name] = path
			}
		}
	}
}
