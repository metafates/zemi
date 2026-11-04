package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
		Name: "plugin", NArgs: NArgsAny, Desc: "install plugins from marketplaces and turn them on or off",
		GoFn: (*Host).pluginsCommand,
		GoComplete: SubcommandCompleter([]string{"new", "modify", "marketplace", "install", "add", "enable", "disable", "delete", "toggle", "open"}, func(h *Host, sub string) []string {
			switch sub {
			case "new":
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
			ps, _, _ := plugin.Scan(h.Dirs)
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
func (h *Host) pluginsCommand(line string, confirm bool) error {
	typed, attached := agent.CutAttached(line)
	args, verb := strings.Fields(typed), ""
	if len(args) > 0 {
		verb = args[0]
	}
	// a delete typed without --yes asks first (AskFirst); an armed key's
	// press is the consent the flag would carry. Other verbs take no --yes.
	ask := false
	if verb == "delete" {
		rest, yes := ConfirmArgs(typed, confirm)
		args = strings.Fields(rest)
		ask = len(args) == 2 && !yes
	}
	// new and modify only send, so they run before the UI attaches too
	ui, noUI := h.Screen()
	switch {
	case noUI == nil || ask || verb == "new" || verb == "modify":
	case verb == "open":
		return noUI
	case verb == "toggle" && len(args) == 2:
		return fmt.Errorf("%w; zemi plugin enable or disable %s from the shell", noUI, args[1])
	default:
		return fmt.Errorf("%w; zemi plugin manages them from the shell", noUI)
	}
	send := func(prompt string) error {
		return h.sendCommandPrompt("plugin", strings.TrimSpace(typed), agent.Attached("command", prompt), attached)
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
	case verb == "marketplace":
		switch {
		case len(args) > 1 && args[1] == "list":
			// file reads only, and the list it refreshes shows the same rows
			if err := plugin.Run(context.Background(), h.Dirs, args, io.Discard); err != nil {
				return pluginsErr(usage, err)
			}
			h.GoSafe("plugin", func() { h.openPlugins(ui, true, "") })
			return nil
		case len(args) == 2 && !slices.Contains(marketplaceVerbs, args[1]):
			// the drill-down a marketplace row's enter opens; the build reads the
			// clone's manifest, so it runs off the key loop, and a gone
			// marketplace opens on its removed row
			h.GoSafe("plugin", func() { h.openMarketplace(ui, args[1]) })
		case len(args) > 1:
			sub := args[1]
			if !slices.Contains(marketplaceVerbs, sub) {
				return pluginsErr(usage, plugin.ErrUsage)
			}
			background(marketplaceLabel(args), func(ctx context.Context) error {
				var out strings.Builder
				err := plugin.Run(ctx, h.Dirs, args, &out)
				body := strings.TrimSpace(out.String())
				title := "Marketplace updated"
				switch sub {
				case "add":
					title = "Marketplace added"
				case "remove":
					title = "Marketplace removed"
				}
				if body == plugin.MarketplacesUpToDate {
					title, body = body, ""
				}
				if err == nil || body != "" {
					h.Notify(slog.LevelInfo, title, body)
				}
				return err
			})
		default:
			return pluginsErr(verb, plugin.ErrUsage)
		}
	case verb == "install" || verb == "add" || verb == "delete":
		// the last word names the busy line either way
		name := "plugin"
		if len(args) > 1 {
			name = plugin.Name(args[len(args)-1])
		}
		verbLabel := "Installing "
		if verb == "delete" {
			verbLabel = "Deleting "
		}
		label := verbLabel + name
		background(label, func(ctx context.Context) error {
			var out strings.Builder
			err := plugin.Run(ctx, h.Dirs, args, &out)
			title := "Plugin installed"
			if verb == "delete" {
				title = "Plugin deleted"
			}
			body := strings.TrimSpace(out.String())
			if verb == "delete" && err == nil {
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

// marketplaceVerbs are the marketplace subcommands that run in the
// background: the build reads the clone's manifest, so it runs off the key
// loop.
var marketplaceVerbs = []string{"add", "remove", "update"}

// marketplaceLabel is the busy line a marketplace verb shows while it runs:
// the clone decides a marketplace's name, so add names the source it was
// given; the others name their argument.
func marketplaceLabel(args []string) string {
	kind := "Updating"
	switch args[1] {
	case "add":
		kind = "Adding"
	case "remove":
		kind = "Removing"
	}
	if len(args) > 2 {
		return kind + " marketplace " + args[2]
	}
	return kind + " marketplaces"
}

// pluginsErr turns a usage error into verb's own grammar, which fits a
// toast, or the whole of it for a verb /plugin does not take.
func pluginsErr(verb string, err error) error {
	if !errors.Is(err, plugin.ErrUsage) {
		return err
	}
	all := strings.TrimPrefix(plugin.ErrUsage.Error(), "usage: plugin ") + " | new [what it does] | modify <name> [what to change] | toggle <name> | open <name>"
	var forms []string
	for form := range strings.SplitSeq(all, " | ") {
		if rest, ok := strings.CutPrefix(form, verb+" "); ok {
			forms = append(forms, rest)
		}
	}
	if len(forms) > 0 {
		return Usagef("usage: /plugin %s %s", verb, strings.Join(forms, " | "))
	}
	return Usagef("usage: /plugin %s", all)
}

// pluginNamed finds a plugin by name, a local one first.
func (h *Host) pluginNamed(name string) (plugin.Plugin, error) {
	ps, _, err := plugin.Scan(h.Dirs)
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
		Reload: h.pluginItems, BodyMD: true,
	})
}

// pluginItems lists the plugins and marketplaces under the list's action rows,
// which carry the empty state, so a rebuild keeps it true.
func (h *Host) pluginItems() []OverlayItem {
	ps, skipped, err := plugin.List(context.Background(), h.Dirs)
	if err != nil {
		h.Notify(slog.LevelError, "Could not read the plugins", err.Error())
	}
	ms, mktErr := plugin.Marketplaces(h.Dirs)
	if mktErr != nil {
		h.Notify(slog.LevelError, "Could not read the marketplaces", mktErr.Error())
	}
	install := OverlayItem{Text: "Install a plugin", Verb: VerbFill, Fill: "/plugin install "}
	if len(ps) == 0 && err == nil {
		install.Desc = "no plugins yet"
	}
	addMarket := OverlayItem{Text: "Add a marketplace", Verb: VerbFill, Fill: "/plugin marketplace add "}
	if len(ms) == 0 && mktErr == nil {
		addMarket.Desc = "no marketplaces yet"
	}
	items := []OverlayItem{{Text: "New plugin", Verb: VerbFill, Fill: "/plugin new "}, install, addMarket}
	provides := h.pluginProvides()
	// Scan reads local/ before installed/, and so does skipped; a dir the name
	// rule refused never loads, so name it where its author looks for it,
	// beside its layer's rows and the section header they share
	layer, next := true, 0
	skips := func(of bool) {
		for ; next < len(skipped) && skipped[next].Local == of; next++ {
			s := skipped[next]
			section := "Installed"
			if of {
				section = "Local"
			}
			items = append(items, OverlayItem{Text: s.String(), Section: section})
		}
	}
	for _, p := range ps {
		if p.Local != layer {
			skips(layer)
			layer = p.Local
		}
		status, loadErr := h.pluginStatus(p)
		var desc []string
		if status == StatusFailed {
			desc = append(desc, string(status))
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
		if p.Enabled {
			keys = append(keys, ItemKey{Key: "d", Desc: "disable", Run: "plugin disable " + p.Name})
		} else {
			keys = append(keys, ItemKey{Key: "e", Desc: "enable", Run: "plugin enable " + p.Name})
		}
		keys = append(keys, ItemKey{Key: "o", Desc: openInEditorLabel, Run: "plugin open " + p.Name})
		if p.Local {
			keys = append(keys, ItemKey{Key: "m", Desc: "modify with zemi", Fill: "/plugin modify " + p.Name + " "})
		}
		keys = append(keys, ItemKey{Key: "x", Desc: "delete", Danger: true, Run: "plugin delete " + p.Name})
		readme := plugin.Readme(p.Dir)
		fields := fieldList("source", source, "status", string(status), "version", version,
			"marketplace", p.Marketplace, "provides", strings.Join(provides[p.Name], "  "))
		// metadata the row shows only when it has it; with a README the
		// description and grants move here from the prose it replaces
		extra := fieldList("author", p.Author, "homepage", p.Homepage)
		if readme != "" {
			extra = append(extra, fieldList("description", p.Desc, "grants", strings.Join(p.Uses, ", "))...)
		}
		for _, f := range extra {
			if f.Value != "" {
				fields = append(fields, f)
			}
		}
		fields = append(fields, fieldList("path", perm.TildePath(p.Dir), "error", errText)...)
		items = append(items, OverlayItem{
			Text: p.Name, Desc: strings.Join(slices.DeleteFunc(desc, func(s string) bool { return s == "" }), "  "),
			Section: section, On: new(p.Enabled), Run: "plugin toggle " + p.Name,
			Detail: cmp.Or(readme, pluginDetail(p)), Keys: keys, Fields: fields,
		})
	}
	skips(layer)
	skips(!layer)
	items = append(items, marketplaceRows(ms)...)
	return items
}

// marketplaceRows lists the Marketplaces section: one row per registered
// marketplace, whose enter opens its drill-down.
func marketplaceRows(ms []plugin.MarketplaceInfo) []OverlayItem {
	rows := make([]OverlayItem, 0, len(ms))
	for _, m := range ms {
		// the source lives on the drill-down's info row, where it fits
		desc := plugin.Count(len(m.Entries))
		if m.Err != "" {
			desc = m.Err
		}
		rows = append(rows, OverlayItem{
			Text: m.Name, Section: "Marketplaces",
			Desc: desc,
			Verb: VerbOpen, Run: "plugin marketplace " + m.Name,
			Keys: []ItemKey{
				{Key: "u", Desc: "update", Run: "plugin marketplace update " + m.Name},
				{Key: "x", Desc: "remove", Danger: true, Run: "plugin marketplace remove " + m.Name},
			},
		})
	}
	return rows
}

// marketplaceItemsOf builds a marketplace's drill-down: an info row, the
// marketplace's own actions, then one row per entry its manifest lists,
// sectioned by the category each names, what install picks from.
func (h *Host) marketplaceItemsOf(name string) []OverlayItem {
	ms, err := plugin.Marketplaces(h.Dirs)
	if err != nil {
		h.Notify(slog.LevelError, "Could not read the marketplaces", err.Error())
	}
	i := slices.IndexFunc(ms, func(m plugin.MarketplaceInfo) bool { return m.Name == name })
	if i < 0 {
		return []OverlayItem{{Text: "Marketplace removed", Desc: "no marketplace is named " + name}}
	}
	m := ms[i]
	if m.Err != "" {
		return []OverlayItem{{Text: "Could not read the manifest", Desc: m.Err}}
	}
	installed := plugin.InstalledEntries(h.Dirs, name)
	live, provs := h.pluginProvides(), plugin.EntryProvides(h.Dirs, name)
	items := []OverlayItem{{
		Text: perm.TildePath(m.Source), Desc: fmt.Sprintf("added %s  %s", m.Added.Format(time.DateOnly), plugin.Count(len(m.Entries))),
		Section: "Marketplace",
		Keys: []ItemKey{
			{Key: "u", Desc: "update", Run: "plugin marketplace update " + name},
			{Key: "x", Desc: "remove", Danger: true, Run: "plugin marketplace remove " + name},
		},
	}}
	// entries section by their manifest category, the order the manifest
	// names them in; no categories at all keeps one Plugins section
	grouped := slices.ContainsFunc(m.Entries, func(e plugin.MarketplaceEntry) bool { return e.Category != "" })
	var order []string
	groups := map[string][]plugin.MarketplaceEntry{}
	for _, e := range m.Entries {
		cat := e.Category
		switch {
		case !grouped:
			cat = "Plugins"
		case cat == "":
			cat = "Other"
		}
		if _, ok := groups[cat]; !ok {
			order = append(order, cat)
		}
		groups[cat] = append(groups[cat], e)
	}
	if !grouped {
		order = []string{"Plugins"}
	}
	for _, cat := range order {
		for _, e := range groups[cat] {
			desc := make([]string, 0, 3)
			if e.Version != "" {
				desc = append(desc, e.Version)
			}
			if p, ok := installed[e.Name]; ok {
				desc = append(desc, "installed as "+p)
			}
			if e.Description != "" {
				desc = append(desc, cutLine(e.Description))
			}
			item := OverlayItem{
				Text: e.Name, Section: cat,
				Desc:   strings.Join(desc, "  "),
				Detail: e.Description,
			}
			// provides reads the live catalog for an installed entry, the
			// entry's on-disk material for the rest; one whose material was
			// never fetched says so rather than reading as "provides nothing"
			prov := "not installed"
			if inst, ok := installed[e.Name]; ok {
				prov = strings.Join(live[inst], "  ")
			} else if p, ok := provs[e.Name]; ok {
				prov = strings.Join(p, "  ")
			}
			for _, f := range fieldList("version", e.Version, "category", e.Category, "provides", prov,
				"author", e.Author, "homepage", e.Homepage, "grants", strings.Join(e.Uses, ", ")) {
				if f.Value != "" {
					item.Fields = append(item.Fields, f)
				}
			}
			if _, ok := installed[e.Name]; ok {
				item.Keys = nil // an installed one moves with its marketplace's update
			} else {
				run := "plugin install " + e.Name + "@" + name
				item.Keys = []ItemKey{{Key: "i", Desc: "install", Run: run}}
			}
			items = append(items, item)
		}
	}
	return items
}

// openMarketplace opens a marketplace's drill-down, or replaces it in place
// after one of its actions ran.
func (h *Host) openMarketplace(ui UIHooks, name string) {
	ui.OpenOverlay(Overlay{
		ID: "plugin-marketplace", Title: "Marketplace " + name, Origin: lua.OriginBuiltin,
		Items: h.marketplaceItemsOf(name), Reload: func() []OverlayItem { return h.marketplaceItemsOf(name) },
		BodyMD: true,
	})
}

// cutLine is a description's first line, cut where a row's desc stops being
// readable.
func cutLine(s string) string {
	if i := strings.IndexAny(s, "\n.。"); i >= 0 {
		s = s[:i]
	}
	return s
}

// pluginGloss is what each declared name grants, for the detail pane: the same
// words as the skill's table, short enough to sit in one line of prose.
var pluginGloss = map[string]string{
	"model":        "a model or classify call",
	"agent":        "a message to the agent",
	"http":         "a fetch",
	"job":          "a command it starts",
	"fs.write":     "a file write",
	"tool":         "a tool it registers being called",
	plugin.CapPerm: "steering the permission gate",
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

// pluginProvides lists what each plugin provides, by plugin name. PluginOf
// resolves a component's file and the grant roots, so a linked install's
// material (its path runs through marketplaces/) attributes to the install
// it links from.
func (h *Host) pluginProvides() map[string][]string {
	out := map[string][]string{}
	add := func(path, what string) {
		if name := h.PluginOf(path); name != "" {
			out[name] = append(out[name], what)
		}
	}
	for _, c := range h.Commands.List() {
		add(c.Path, "command /"+c.Name)
	}
	for _, t := range h.luaTools.list() {
		add(t.Path, "tool "+t.Name)
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
