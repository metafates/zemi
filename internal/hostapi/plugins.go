package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/set"
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
// Claude-layout (their layered names fall back to a prefix and their MCP
// servers and hooks load). One swap, so a reload, a disable or a delete
// cannot leave a stale grant.
type pluginGrant struct {
	roots  []string
	names  []string            // roots[i] is the install of names[i]
	uses   map[string][]string // by plugin name
	claude set.Set[string]     // root -> it is a Claude-layout plugin
}

// PluginsStep loads every enabled plugin's init.lua as plugin origin, sorted
// by name (DESIGN §5), after fixing the generation's plugin roots, so one
// plugin may require another's modules. A failing plugin does not stop the
// rest. It runs no git.
func (h *Host) PluginsStep() lua.BootStep {
	return lua.BootStep{
		Name: "plugins",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			ps, _, err := plugin.Scan(h.Dirs)
			if err == nil {
				// a failed scan lists no plugins, and the sweep would take every
				// plugin's saved tokens with it
				h.sweepPluginState(ps)
			}
			if err := plugin.EnsureLinks(h.Dirs); err != nil {
				h.Log.Warn("installs not re-linked", "origin", "plugins", "error", err)
			}
			if err == nil {
				ps, _, err = plugin.Scan(h.Dirs)
				for _, p := range ps {
					if p.ManifestErr != nil {
						h.RecordErrorOnce("plugin", p.Name, p.ManifestErr.Error())
					}
				}
			}
			run, loads := pickPlugins(ps, h.pluginsRoot())
			grant := pluginGrant{roots: make([]string, len(run)), names: make([]string, len(run)), uses: map[string][]string{}, claude: set.Set[string]{}}
			hooks := map[string][]hookEntry{}
			for i, p := range run {
				kind := plugin.DirInstalled
				if p.Local {
					kind = plugin.DirLocal
				}
				// resolved, as the chunk paths are: an installed plugin is a
				// symlink into its marketplace material
				root := perm.RealPath(filepath.Join(h.pluginsRoot(), kind, p.Name))
				grant.roots[i], grant.names[i] = root, p.Name
				grant.uses[p.Name] = p.Uses
				// zemi's own tree wins: a plugin carrying .zemi loads none of
				// the Claude side (its servers, hooks and name prefixes)
				// the install path, not the resolved root: the entry-carried
				// manifest is keyed by install name, and the file leg reads
				// through the link
				raw := plugin.ClaudeRaw(h.Dirs, p.Dir)
				if raw == nil {
					continue
				}
				if _, err := os.Stat(filepath.Join(root, plugin.CodeDir)); err == nil {
					continue
				}
				grant.claude.Add(root)
				// the parts of the layout zemi has no use for yet (§6 v1 cuts)
				for _, part := range []string{"output-styles", ".lsp.json", "settings.json"} {
					if _, err := os.Stat(filepath.Join(root, part)); err == nil {
						h.Log.Warn("plugin part zemi does not load, skipped", "origin", "plugins", "plugin", p.Name, "part", part)
					}
				}
				data := h.pluginDataDir(p.Name)
				if mkErr := os.MkdirAll(data, 0o755); mkErr != nil {
					h.Log.Warn("plugin data dir not created", "origin", "plugins", "plugin", p.Name, "error", mkErr)
				}
				m, rerr := plugin.ReadClaudeRaw(root, data, raw)
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
				loads = append(loads, pluginLoad{name: p.Name, dir: p.Dir, err: lua.LoadFileStep("plugin "+p.Name, init, lua.OriginPlugin).Run(ctx, r, g)})
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
// the loader runs and those it refuses, with why. An installed symlink runs
// when its target stays inside the plugins tree, where origin classifies its
// files as plugin code; a link pointing out would run under nobody's class.
// The root is resolved, since the target is.
func pickPlugins(ps []plugin.Plugin, pluginsRoot string) (run []plugin.Plugin, refused []pluginLoad) {
	if r, err := filepath.EvalSymlinks(pluginsRoot); err == nil {
		pluginsRoot = r
	}
	installed := set.Set[string]{}
	for _, p := range ps {
		if !p.Local {
			installed.Add(p.Name)
		}
	}
	slices.SortStableFunc(ps, func(a, b plugin.Plugin) int { return cmp.Compare(a.Name, b.Name) })
	for _, p := range ps {
		fi, err := os.Lstat(p.Dir)
		uses := plugin.CheckUses(p.Uses)
		isLink := err == nil && fi.Mode()&fs.ModeSymlink != 0
		var target string
		if isLink {
			target, err = filepath.EvalSymlinks(p.Dir)
		}
		// the old layout is refused for a real dir only; a link skips it
		var old error
		if err == nil && !isLink && fi.IsDir() {
			old = plugin.OldLayout(p.Dir)
		}
		switch {
		case !p.Enabled:
		case uses != nil:
			refused = append(refused, pluginLoad{name: p.Name, dir: p.Dir, err: uses})
		case p.Local && installed.Has(p.Name):
			refused = append(refused, pluginLoad{name: p.Name, dir: p.Dir, err: errors.New("an installed plugin has this name; delete it or rename this one")})
		case err != nil:
			refused = append(refused, pluginLoad{name: p.Name, dir: p.Dir, err: errors.New("a dangling link, which loads nothing; it may have left its marketplace; delete it, then install it again")})
		case isLink && !strings.HasPrefix(target, pluginsRoot+string(filepath.Separator)):
			why := "a link out of the plugins tree, which zemi does not load; install it from its marketplace instead"
			if p.Local {
				why = "a link out of the plugins tree, which zemi does not load; keep it in " + filepath.Dir(p.Dir) + " as a real directory"
			}
			refused = append(refused, pluginLoad{name: p.Name, dir: p.Dir, err: errors.New(why)})
		case old != nil:
			refused = append(refused, pluginLoad{name: p.Name, dir: p.Dir, err: old})
		default:
			run = append(run, p)
		}
	}
	return run, refused
}

// PluginDir is the directory of the plugin named name that the plugins step
// would load, turned off or not, or why it would load none.
func PluginDir(d config.Dirs, name string) (string, error) {
	ps, _, err := plugin.Scan(d)
	if err != nil {
		return "", err
	}
	for i := range ps {
		ps[i].Enabled = true
	}
	run, refused := pickPlugins(ps, d.PluginsRoot())
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
// symlink-resolved like the chunk paths they name.
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
	return g != nil && g.claude.Has(root)
}

// claudeRootOf is the Claude-layout plugin root path belongs to, or "".
func (h *Host) claudeRootOf(path string) string {
	sep := string(filepath.Separator)
	var best string
	if g := h.pluginGrant.Load(); g != nil {
		// the longest root wins, so a material nested in another plugin's
		// expands against its own; "" (an unresolvable root) is inside nothing
		for root := range g.claude {
			if root == "" || (path != root && !strings.HasPrefix(path, root+sep)) {
				continue
			}
			if best == "" || len(root) > len(best) {
				best = root
			}
		}
	}
	return best
}

// pluginPrefix is the fallback name of a Claude-layout plugin's layered
// resources: its sanitized name and a dash, joined when the bare name is
// already taken.
func (h *Host) pluginPrefix(root string) string {
	return plugin.SanitizeName(h.PluginOf(root)) + "-"
}

// layerPrefix is the fallback name prefix of a Claude-layout plugin's
// layered files; workflows stay flat, their names no user types.
func (h *Host) layerPrefix(root, sub string) string {
	if sub == "workflows" || !h.isClaudePlugin(root) {
		return ""
	}
	return h.pluginPrefix(root)
}

// claudeCommandSkills turns a Claude plugin's commands/*.md into
// model-invocable skill rows named by their stem (invoked with @name like
// every skill; the sync falls back to the plugin-prefixed name on a clash):
// Claude loads commands and skills as one thing, zemi's /command stays
// mdcommands'. A name that would break skill.ValidName is skipped, warned by
// the command load.
func (h *Host) claudeCommandSkills(root, data string) []skill.Skill {
	entries, err := os.ReadDir(filepath.Join(root, "commands"))
	if err != nil {
		return nil
	}
	var rows []skill.Skill
	for _, e := range entries {
		stem, ok := strings.CutSuffix(e.Name(), ".md")
		if !ok || e.IsDir() || !skill.ValidName(stem) {
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
			Name: stem, Dir: filepath.Join(root, "commands"), Source: skill.Plugin,
			Description: sanitize.Truncate(desc, skill.MaxDescriptionRunes),
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
		spec := mcpSpec{
			Command: s.Command, Args: s.Args, URL: s.URL, Headers: s.Headers,
			ClientID: s.ClientID, ClientSecret: s.ClientSecret, CallbackPort: s.CallbackPort,
		}
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

// pluginDeclares reports whether the plugin named name declared capability in
// the manifest of the generation that loaded it.
func (h *Host) pluginDeclares(name, capability string) bool {
	g := h.pluginGrant.Load()
	return g != nil && name != "" && slices.Contains(g.uses[name], capability)
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
	ids, fresh, adopt := map[string]string{}, set.Set[string]{}, set.Set[string]{}
	for _, p := range ps {
		q := strconv.Quote(installID(h.Dirs, p.Name, p.Dir))
		was, err := h.GlobalDB.GetKV(perm.PluginPrefix + p.Name)
		switch {
		case errors.Is(err, store.ErrNotFound) && q != `""`:
			// a copy's id lives in its row alone: adopt it on first sight, or
			// the id an update backfilled into the row would wipe the state
			was = q
			adopt.Add(p.Name)
		case errors.Is(err, store.ErrNotFound):
			was = `""` // no id kept: a local plugin, or a clone no sweep has seen
		case err != nil:
			was = q // a failed read must not drop the plugin's keys
		}
		ids[p.Name] = q
		if was != q {
			fresh.Add(p.Name)
		}
	}
	keys, _ := h.GlobalDB.KeysKV(perm.PluginPrefix)
	picks, _ := h.GlobalDB.KeysKV("config:plugins.")
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	for _, k := range append(keys, picks...) {
		// a name holds no "." or ":"; its state keys are plugin:<name>:<key>,
		// its kept settings config:plugins.<name>.<key>
		name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(k, perm.PluginPrefix), "config:plugins."), ":")
		name, _, _ = strings.Cut(name, ".")
		if _, ok := ids[name]; ok && !fresh.Has(name) {
			continue
		}
		if path, ok := strings.CutPrefix(k, kvConfigPrefix); ok {
			h.forgetPick(config.Key(path))
		} else {
			_, _ = h.GlobalDB.DelKV(k)
		}
	}
	for name, id := range ids {
		if fresh.Has(name) && id != `""` || adopt.Has(name) {
			_ = h.GlobalDB.SetKV(perm.PluginPrefix+name, id)
		}
	}
}

// installID is the identity the sweep compares for the plugin name in dir:
// the marketplace row's, which a copy has and a git install shares with its
// .git, else the clone's own.
func installID(d config.Dirs, name, dir string) string {
	if reg, ok := plugin.MarketplaceOf(d, name); ok && reg.ID != "" {
		return reg.ID
	}
	return plugin.InstallID(dir)
}

// localPlugins holds the plugins the user develops in place: the prompt
// names it.
func (h *Host) localPlugins() string {
	return filepath.Join(h.Dirs.PluginsRoot(), plugin.DirLocal)
}

// PluginOf names the plugin a file belongs to, its directory under
// installed/ or local/, or "" for any other path. The input and the grant
// roots are resolved first: a linked install's files carry their marketplace
// material path, which the grant maps back to the install it loaded.
func (h *Host) PluginOf(path string) string {
	if path == "" {
		return ""
	}
	if resolved := perm.RealPath(path); resolved != "" {
		path = resolved
	}
	sep := string(filepath.Separator)
	if rel, ok := strings.CutPrefix(path, h.pluginsRoot()+sep); ok {
		kind, rest, _ := strings.Cut(rel, sep)
		name, _, _ := strings.Cut(rest, sep)
		if kind == plugin.DirLocal || kind == plugin.DirInstalled {
			return name
		}
	}
	if g := h.pluginGrant.Load(); g != nil {
		// the longest root wins, so a material nested in another plugin's
		// attributes to its own; "" (an unresolvable root, a symlink loop)
		// is inside nothing
		best := -1
		for i, root := range g.roots {
			if root == "" || (path != root && !strings.HasPrefix(path, root+sep)) {
				continue
			}
			if best < 0 || len(root) > len(g.roots[best]) {
				best = i
			}
		}
		if best >= 0 {
			return g.names[best]
		}
	}
	return ""
}

// OriginBadge marks what a plugin or the project's files draw or run
// ("plugin:jira", "project:fix-lint.md"), "" for the user's and zemi's own: a
// plugin's name, else the file's, since a path leaves no room for a title.
func (h *Host) OriginBadge(o lua.Origin, path string) string {
	switch {
	case o == lua.OriginBuiltin || o == lua.OriginUser:
		return ""
	case h.PluginOf(path) != "":
		return string(o) + ":" + h.PluginOf(path)
	case path != "":
		return string(o) + ":" + filepath.Base(path)
	default:
		return string(o)
	}
}

// localPluginDir is the local/<name> directory holding path, or "".
func (h *Host) localPluginDir(path string) string {
	name := h.PluginOf(path)
	if dir := filepath.Join(h.pluginsRoot(), plugin.DirLocal, name); name != "" && strings.HasPrefix(path, dir) {
		return dir
	}
	return ""
}

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

// layerFiles hands load the <name><ext> files in sub of the user config, each
// enabled plugin, then a trusted project's .agents - workflows never read the
// project layer, project Lua being dead. The first file of a name load
// accepts wins, so a broken one hides nothing, and a Claude plugin's bare
// name taken by an earlier file retries the plugin-prefixed one; /errors
// records a name no skill.ValidName and one two plugins still clash on.
func (h *Host) layerFiles(sub, ext string, load func(layerFile) bool) {
	var dirs []layerFile
	if h.Dirs.ConfigHome != "" {
		dirs = append(dirs, layerFile{path: filepath.Join(h.Dirs.ConfigHome, sub), origin: lua.OriginUser})
	}
	for _, p := range h.pluginDirs() {
		dirs = append(dirs, layerFile{path: pluginLayerDir(p, sub), origin: lua.OriginPlugin, prefix: h.layerPrefix(p, sub)})
	}
	if pdir := h.Dirs.ProjectConfigDir(); sub != "workflows" && pdir != "" && h.ProjectTrusted() {
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
			path := filepath.Join(d.path, e.Name())
			// the bare name first; a Claude plugin's falls back to its
			// prefixed name only when another file already holds the bare one
			bare, renamed := name, false
			if _, taken := seen[name]; taken && d.prefix != "" {
				name, renamed = d.prefix+name, true
			}
			if kept, taken := seen[name]; taken {
				if a, b := h.PluginOf(kept), h.PluginOf(path); a != "" && b != "" && a != b {
					h.RecordErrorOnce("plugins", "", fmt.Sprintf("%s %s is already provided by plugin %s, not %s", strings.TrimSuffix(sub, "s"), name, a, b))
				}
				continue
			}
			if !skill.ValidName(name) {
				h.RecordErrorOnce(sub, "", path+": name must be "+skill.NameRule)
				continue
			}
			if load(layerFile{name: name, path: path, origin: d.origin, prefix: d.prefix}) {
				seen[name] = path
				if renamed {
					h.Log.Warn(fmt.Sprintf("%s %s is already in %s, loaded as %s", strings.TrimSuffix(sub, "s"), bare, seen[bare], name), "origin", sub, "path", path)
				}
			}
		}
	}
}
