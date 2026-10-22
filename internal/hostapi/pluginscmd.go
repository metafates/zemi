package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/plugin"
)

// newPluginPrompt rides behind /plugin new and modifyPluginPrompt behind
// /plugin modify; the procedure lives in the zemi skill.
const (
	newPluginPrompt    = "I want a new zemi plugin that does what the command above says; ask me when it says nothing. Read SKILL.md in %s, the zemi skill's directory, then follow the Plugin steps in plugins.md there: tests, not this session, are the check."
	modifyPluginPrompt = "Change my zemi plugin %s in %s as the command above says; ask me when it says nothing. Read SKILL.md in %s, the zemi skill's directory, then follow the Plugin steps in plugins.md there: tests, not this session, are the check."
)

// pluginsCommands is /plugin: the zemi plugin verbs, new and modify, which
// send the agent a prompt, and toggle and open.
func (h *Host) pluginsCommands() []*Command {
	return []*Command{{
		Name: "plugin", NArgs: NArgsAny, Desc: "install, update and turn plugins on or off",
		GoFn: (*Host).pluginsCommand,
		GoComplete: SubcommandCompleter([]string{"new", "modify", "marketplace", "install", "add", "update", "check", "enable", "disable", "delete", "toggle", "open"}, func(h *Host, sub string) []string {
			switch sub {
			case "new", "check":
				return nil
			case "install", "add":
				// the entries a registered marketplace lists, as install types them
				ms, _ := plugin.Marketplaces(h.Dirs)
				var out []string
				for _, m := range ms {
					for _, e := range m.Entries {
						out = append(out, e.Name+"@"+m.Name)
					}
				}
				return out
			case "marketplace":
				return []string{"add", "list", "remove", "update"}
			}
			ps, _ := plugin.Scan(h.Dirs)
			var names []string
			for _, p := range ps {
				if sub != "modify" || p.Local {
					names = append(names, p.Name)
				}
			}
			return names
		}),
	}}
}

// pluginsCommand is /plugin; new and modify send the line as typed,
// newlines included, with their prompt riding behind it.
func (h *Host) pluginsCommand(line string) error {
	typed, stdin := agent.CutAttached(line)
	args, verb := strings.Fields(typed), ""
	if len(args) > 0 {
		verb = args[0]
	}
	// a delete typed without --yes asks first (AskFirst)
	ask := verb == "delete" && len(args) == 2
	if verb == "delete" && len(args) == 3 && args[2] == "--yes" {
		args = args[:2]
	}
	// new and modify only send, so zemi run takes them
	ui, noUI := h.Screen()
	switch {
	case noUI == nil || ask || verb == "new" || verb == "modify":
	case verb == "open":
		return noUI
	default:
		return fmt.Errorf("%w; zemi plugin manages them from the shell", noUI)
	}
	send := func(prompt string) error {
		return h.AgentSend("/plugin " + strings.TrimSpace(typed) + agent.Attached("command", prompt) + stdin)
	}
	// git, or a big RemoveAll: off the key loop as a busy job, then the
	// list again
	usage := verb
	if verb == "marketplace" && len(args) > 1 {
		usage = "marketplace " + args[1] // the subverb's own grammar
	}
	background := func(label string, fn func(ctx context.Context) error) {
		h.GoSafe("plugin "+verb, func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stop := cancel
			if verb == "delete" {
				stop = nil // Delete takes no ctx, so esc would stop nothing
			}
			err := func() error {
				defer h.Busy(label, stop)()
				return fn(ctx)
			}()
			if err != nil && ctx.Err() == nil {
				h.CommandFailed("plugin", pluginsErr(usage, err))
				if errors.Is(err, plugin.ErrUsage) {
					return
				}
			}
			h.openPlugins(ui, true, "")
		})
	}
	switch {
	case len(args) == 0:
		h.GoSafe("plugin", func() { h.openPlugins(ui, false, "") })
	case verb == "toggle" && len(args) == 2:
		p, err := h.pluginNamed(args[1])
		if err != nil {
			return err
		}
		return pluginsErr(verb, plugin.SetEnabled(h.Dirs, p.Name, !p.Enabled))
	case verb == "open" && len(args) == 2:
		p, err := h.pluginNamed(args[1])
		if err != nil {
			return err
		}
		// one that ships only skills or commands has no init.lua to create
		path := filepath.Join(p.Dir, plugin.InitFile)
		if _, err := os.Stat(path); err != nil {
			path = p.Dir
		}
		h.openInEditor(ui, path, "the plugin")
	case verb == "new":
		return send(fmt.Sprintf(newPluginPrompt, h.zemiSkillDir()))
	case verb == "modify" && len(args) >= 2:
		p, err := h.pluginNamed(args[1])
		if err != nil {
			return err
		}
		if !p.Local {
			return Usagef("%s is an installed plugin, and the agent changes only local ones: delete it and ask the agent to clone it into the local plugins directory", p.Name)
		}
		if fi, err := os.Lstat(p.Dir); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return Usagef("%s is a symlink, which zemi does not load: move the plugin into %s, then modify it", p.Name, perm.TildePath(filepath.Dir(p.Dir)))
		}
		// the user asked for the change: in ask mode the gate still shows
		// each edit, and that ask is the consent
		return send(fmt.Sprintf(modifyPluginPrompt, p.Name, perm.TildePath(p.Dir), h.zemiSkillDir()))
	case ask:
		p, err := h.pluginNamed(args[1])
		if err != nil {
			return err
		}
		line := "plugin delete " + p.Name + " --yes"
		return AskFirst(line, "zemi "+line, func(confirm string) error {
			if noUI != nil {
				return noUI
			}
			h.GoSafe("plugin", func() { h.openPlugins(ui, false, confirm) })
			return nil
		})
	case verb == "enable" || verb == "disable":
		// at once, so the list an action returns to shows it
		return pluginsErr(verb, plugin.Run(context.Background(), h.Dirs, args, io.Discard))
	case verb == "marketplace" && len(args) > 1 && args[1] == "list":
		// file reads only, and the list it refreshes shows the same rows
		if err := plugin.Run(context.Background(), h.Dirs, args, io.Discard); err != nil {
			return pluginsErr(usage, err)
		}
		h.GoSafe("plugin", func() { h.openPlugins(ui, true, "") })
		return nil
	case verb == "marketplace" && len(args) > 1:
		sub := args[1]
		if sub != "add" && sub != "remove" && sub != "update" {
			return pluginsErr(usage, plugin.ErrUsage)
		}
		label := map[string]string{"add": "Adding marketplace", "remove": "Removing marketplace", "update": "Updating marketplaces"}[sub]
		if sub == "remove" && len(args) > 2 {
			label += " " + args[2]
		}
		background(label, func(ctx context.Context) error {
			var out strings.Builder
			err := plugin.Run(ctx, h.Dirs, args, &out)
			body := strings.TrimSpace(out.String())
			title := map[string]string{"add": "Marketplace added", "remove": "Marketplace removed", "update": "Marketplace updated"}[sub]
			if body == plugin.MarketplacesUpToDate {
				title, body = body, ""
			}
			if err == nil || body != "" {
				h.Notify(slog.LevelInfo, title, body)
			}
			return err
		})
	case verb == "check" && len(args) == 1:
		background("Checking plugins", func(ctx context.Context) error {
			ps, err := plugin.Check(ctx, h.Dirs)
			var names []string
			for _, p := range ps {
				if p.Latest != "" {
					names = append(names, p.Name)
				}
			}
			if len(names) > 0 {
				h.Notify(slog.LevelInfo, "Updates available", strings.Join(names, ", "))
			} else if err == nil {
				h.Notify(slog.LevelInfo, plugin.UpToDate, "")
			}
			return err
		})
	case verb == "install" || verb == "add" || verb == "update" || verb == "delete":
		// the last word names the busy line either way
		name := "plugin"
		if len(args) > 1 {
			name = plugin.Name(args[len(args)-1])
		}
		label := map[string]string{"install": "Installing ", "add": "Installing ", "update": "Updating ", "delete": "Deleting "}[verb] + name
		background(label, func(ctx context.Context) error {
			var out strings.Builder
			err := plugin.Run(ctx, h.Dirs, args, &out)
			title := map[string]string{"install": "Plugin installed", "add": "Plugin installed", "update": "Plugin updated", "delete": "Plugin deleted"}[verb]
			body := strings.TrimSpace(out.String())
			if body == plugin.UpToDate {
				title, body = body, ""
			} else if verb == "delete" && err == nil {
				body = args[1]
			}
			if err == nil || body != "" {
				h.Notify(slog.LevelInfo, title, body)
			}
			return err
		})
	default:
		return pluginsErr(verb, plugin.ErrUsage)
	}
	return nil
}

// pluginsErr turns a usage error into verb's own grammar, which fits a
// toast, or the whole of it for a verb /plugin does not take.
func pluginsErr(verb string, err error) error {
	if !errors.Is(err, plugin.ErrUsage) {
		return err
	}
	all := strings.TrimPrefix(plugin.ErrUsage.Error(), "usage: plugin ") + " | new [what it does] | modify <name> [what to change] | toggle <name> | open <name>"
	for form := range strings.SplitSeq(all, " | ") {
		if form == verb || strings.HasPrefix(form, verb+" ") {
			return Usagef("usage: /plugin %s", form)
		}
	}
	return Usagef("usage: /plugin %s", all)
}

// pluginNamed finds a plugin by name, a local one first.
func (h *Host) pluginNamed(name string) (plugin.Plugin, error) {
	ps, err := plugin.Scan(h.Dirs)
	if err != nil {
		return plugin.Plugin{}, err
	}
	if i := slices.IndexFunc(ps, func(p plugin.Plugin) bool { return p.Name == name }); i >= 0 {
		return ps[i], nil
	}
	return plugin.Plugin{}, Usagef("no plugin is named %q", name)
}

// openPlugins opens /plugin, or replaces the open list in place; a refresh
// after a slow action opens nothing once the user closed it, and confirm arms
// the action that runs it (Overlay.Confirm). It reads git, so it never runs
// on the key loop.
func (h *Host) openPlugins(ui UIHooks, refresh bool, confirm string) {
	ui.OpenOverlay(Overlay{
		ID: "plugins", Title: "Plugins", Origin: lua.OriginBuiltin, Refresh: refresh, Items: h.pluginItems(), Confirm: confirm,
		Reload: h.pluginItems,
	})
}

// pluginItems lists the plugins under three action rows, which carry the empty
// state, so a rebuild keeps it true.
func (h *Host) pluginItems() []OverlayItem {
	ps, err := plugin.List(context.Background(), h.Dirs)
	if err != nil {
		h.Notify(slog.LevelError, "Could not read the plugins", err.Error())
	}
	install := OverlayItem{Text: "Install a plugin", Verb: VerbFill, Fill: "/plugin install "}
	if len(ps) == 0 && err == nil {
		install.Desc = "no plugins yet"
	}
	items := []OverlayItem{{Text: "New plugin", Verb: VerbFill, Fill: "/plugin new "}, install, {Text: "Check for updates", Verb: VerbRun, Run: "plugin check"}}
	provides := h.pluginProvides()
	for _, p := range ps {
		status, loadErr := h.pluginStatus(p)
		var desc []string
		if status == StatusFailed {
			desc = append(desc, string(status))
		}
		if p.Latest != "" {
			desc = append(desc, "update "+p.Latest)
		}
		source, section, version, errText := p.URL, "Installed", p.Version, ""
		if p.Local {
			source, section = "local", "Local"
			// the user's own code: no manifest and no clone to read a version from
			version = cmp.Or(version, "local")
		}
		desc = append(desc, version)
		if p.Marketplace != "" {
			desc = append(desc, "from "+p.Marketplace)
		}
		if len(p.Uses) > 0 {
			desc = append(desc, "uses: "+strings.Join(p.Uses, ", "))
		}
		desc = append(desc, p.Desc)
		if loadErr != nil {
			errText = loadErr.Error()
		}
		var keys []ItemKey
		if p.Latest != "" && !strings.HasSuffix(p.Latest, "(major)") {
			label := "update to " + p.Latest
			if strings.Contains(p.Latest, "new commit") || strings.HasPrefix(p.Latest, "updated in the marketplace (") {
				label = "update"
			}
			keys = append(keys, ItemKey{Key: "u", Desc: label, Run: "plugin update " + p.Name})
		}
		if p.Enabled {
			keys = append(keys, ItemKey{Key: "d", Desc: "disable", Run: "plugin disable " + p.Name})
		} else {
			keys = append(keys, ItemKey{Key: "e", Desc: "enable", Run: "plugin enable " + p.Name})
		}
		keys = append(keys, ItemKey{Key: "o", Desc: openInEditorLabel, Run: "plugin open " + p.Name})
		if p.Local {
			keys = append(keys, ItemKey{Key: "m", Desc: "modify with zemi", Fill: "/plugin modify " + p.Name + " "})
		}
		keys = append(keys, ItemKey{Key: "x", Desc: "delete", Danger: true, Run: "plugin delete " + p.Name + " --yes"})
		items = append(items, OverlayItem{
			Text: p.Name, Desc: strings.Join(slices.DeleteFunc(desc, func(s string) bool { return s == "" }), " · "),
			Section: section, On: new(p.Enabled), Run: "plugin toggle " + p.Name,
			Detail: pluginDetail(p), Keys: keys,
			Fields: fieldList("source", source, "status", string(status), "version", version, "latest", p.Latest,
				"marketplace", p.Marketplace, "provides", strings.Join(provides[p.Name], " · "), "path", perm.TildePath(p.Dir), "error", errText),
		})
	}
	h.marketplaceItems(&items)
	return items
}

// marketplaceItems appends the Marketplaces section: one row per registered
// marketplace, or the filler row that adds one when none is.
func (h *Host) marketplaceItems(items *[]OverlayItem) {
	ms, err := plugin.Marketplaces(h.Dirs)
	if err != nil {
		h.Notify(slog.LevelError, "Could not read the marketplaces", err.Error())
	}
	if len(ms) == 0 {
		if err == nil {
			*items = append(*items, OverlayItem{
				Text: "Add a marketplace", Verb: VerbFill, Fill: "/plugin marketplace add ",
				Section: "Marketplaces", Desc: "none registered",
			})
		}
		return
	}
	for _, m := range ms {
		count := fmt.Sprintf("%d plugin", len(m.Entries))
		if len(m.Entries) != 1 {
			count += "s"
		}
		*items = append(*items, OverlayItem{
			Text: m.Name, Section: "Marketplaces",
			Desc:   perm.TildePath(m.Source) + " · " + count,
			Detail: marketplaceDetail(m),
			Fields: fieldList("source", perm.TildePath(m.Source), "plugins", strconv.Itoa(len(m.Entries)), "added", m.Added.Format(time.DateOnly)),
			Keys: []ItemKey{
				{Key: "u", Desc: "update", Run: "plugin marketplace update " + m.Name},
				{Key: "x", Desc: "remove", Danger: true, Run: "plugin marketplace remove " + m.Name},
			},
		})
	}
}

// marketplaceDetail is a marketplace row's detail: the entries its clone's
// manifest lists, what install <name>[@<marketplace>] picks from.
func marketplaceDetail(m plugin.MarketplaceInfo) string {
	lines := make([]string, 0, len(m.Entries))
	for _, e := range m.Entries {
		line := e.Name
		if e.Version != "" {
			line += " " + e.Version
		}
		if e.Description != "" {
			line += " - " + e.Description
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "Its manifest lists no plugins."
	}
	return "Installed with /plugin install <name>@" + m.Name + ":\n" + strings.Join(lines, "\n")
}

// pluginGloss is what each declared name grants, for the detail pane: the same
// words as the skill's table, short enough to sit in one line of prose.
var pluginGloss = map[string]string{
	"model":    "a model or classify call",
	"agent":    "a message to the agent",
	"http":     "a fetch",
	"job":      "a command it starts",
	"fs.write": "a file write",
	"tool":     "a tool it registers being called",
}

// pluginDetail is the detail pane's prose: what the plugin ships, the set its
// manifest declares and, while it is enabled, what that grants.
func pluginDetail(p plugin.Plugin) string {
	if len(p.Uses) == 0 {
		return strings.TrimSpace(p.Desc + "\n\nIt declares nothing, so every call zemi gates is refused or asks.")
	}
	glossed := make([]string, len(p.Uses))
	for i, u := range p.Uses {
		if gloss := pluginGloss[u]; gloss != "" {
			u += " (" + gloss + ")"
		}
		glossed[i] = u
	}
	detail := p.Desc + "\n\nDeclares " + strings.Join(glossed, ", ") + ": what switching it on grants."
	if p.Enabled {
		detail += " Anything else it calls is refused, and its writes to its own code, .git," +
			" .agents and zemi's own files still ask."
	}
	return strings.TrimSpace(detail)
}

// pluginStatus is how the last boot left p: "starting" when it is on but no
// boot has run it yet.
func (h *Host) pluginStatus(p plugin.Plugin) (ServiceStatus, error) {
	if !p.Enabled {
		return StatusOff, nil
	}
	if loads := h.pluginLoads.Load(); loads != nil {
		for _, l := range *loads {
			if l.dir != p.Dir {
				continue
			}
			if l.err != nil {
				return StatusFailed, l.err
			}
			return StatusReady, nil
		}
	}
	return StatusStarting, nil
}

// pluginProvides lists what each plugin provides, by plugin name.
func (h *Host) pluginProvides() map[string][]string {
	out := map[string][]string{}
	add := func(path, what string) {
		if n := h.PluginOf(path); n != "" {
			out[n] = append(out[n], what)
		}
	}
	for _, c := range h.Commands.List() {
		add(c.Path, "command /"+c.Name)
	}
	for _, t := range h.luaTools.list() {
		add(t.Path, "tool "+t.Name)
	}
	for _, w := range h.Widgets.List() {
		add(w.Path, "widget "+w.ID)
	}
	for _, k := range h.Keymaps.List() {
		add(k.Path, "keymap "+k.Key)
	}
	for _, s := range h.skillsReg.list() {
		add(s.Dir, "skill "+s.Name)
	}
	for _, d := range h.agentDefs() {
		add(d.Path, "agent "+d.Name)
	}
	for _, w := range h.workflows() {
		add(w.Path, "workflow "+w.Name)
	}
	// a plugin's own settings live at plugins.<its name>.<key> (zemi.config.add)
	for _, s := range h.Settings.Schema() {
		if name := settingPlugin(s.Path); name != "" {
			out[name] = append(out[name], "setting "+string(s.Path))
		}
	}
	return out
}
