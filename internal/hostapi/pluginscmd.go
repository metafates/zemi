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
	"strings"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/plugin"
)

// newPluginPrompt rides behind /plugins new and modifyPluginPrompt behind
// /plugins modify; the procedure lives in the zemi skill.
const (
	newPluginPrompt    = "I want a new zemi plugin that does what the command above says; ask me when it says nothing. Read SKILL.md in %s, the zemi skill's directory, then follow the Plugin steps in plugins.md there: tests, not this session, are the check."
	modifyPluginPrompt = "Change my zemi plugin %s in %s as the command above says; ask me when it says nothing. Read SKILL.md in %s, the zemi skill's directory, then follow the Plugin steps in plugins.md there: tests, not this session, are the check."
)

// pluginsCommands is /plugins: the zemi plugins verbs, new and modify, which
// send the agent a prompt, and toggle and open.
func (h *Host) pluginsCommands() []*Command {
	return []*Command{{
		Name: "plugins", NArgs: NArgsAny, Desc: "install, update and turn plugins on or off",
		GoFn: (*Host).pluginsCommand,
		GoComplete: SubcommandCompleter([]string{"new", "modify", "install", "update", "check", "enable", "disable", "delete", "toggle", "open"}, func(h *Host, sub string) []string {
			if sub == "new" || sub == "install" || sub == "check" {
				return nil
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

// pluginsCommand is /plugins; new and modify send the line as typed,
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
		return fmt.Errorf("%w; zemi plugins manages them from the shell", noUI)
	}
	send := func(prompt string) error {
		return h.AgentSend("/plugins " + strings.TrimSpace(typed) + agent.Attached("command", prompt) + stdin)
	}
	// git, or a big RemoveAll: off the key loop as a busy job, then the
	// list again
	background := func(label string, fn func(ctx context.Context) error) {
		h.GoSafe("plugins "+verb, func() {
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
				h.CommandFailed("plugins", pluginsErr(verb, err))
				if errors.Is(err, plugin.ErrUsage) {
					return
				}
			}
			h.openPlugins(ui, true, "")
		})
	}
	switch {
	case len(args) == 0:
		h.GoSafe("plugins", func() { h.openPlugins(ui, false, "") })
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
		path := filepath.Join(p.Dir, "init.lua")
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
		// the user asked for the change, as for a plugin whose creation they
		// allowed: auto mode settles the agent's edits there until zemi exits
		h.madePlugins.Store(h.localPluginDir(perm.RealPath(p.Dir)), true)
		return send(fmt.Sprintf(modifyPluginPrompt, p.Name, perm.TildePath(p.Dir), h.zemiSkillDir()))
	case ask:
		p, err := h.pluginNamed(args[1])
		if err != nil {
			return err
		}
		line := "plugins delete " + p.Name + " --yes"
		return AskFirst(line, "zemi "+line, func(confirm string) error {
			if noUI != nil {
				return noUI
			}
			h.GoSafe("plugins", func() { h.openPlugins(ui, false, confirm) })
			return nil
		})
	case verb == "enable" || verb == "disable":
		// at once, so the list an action returns to shows it
		return pluginsErr(verb, plugin.Run(context.Background(), h.Dirs, args, io.Discard))
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
	case verb == "install" || verb == "update" || verb == "delete":
		// install takes a URL and an optional name, the last word either way
		name := "plugins"
		if len(args) > 1 {
			name = plugin.Name(args[len(args)-1])
		}
		label := map[string]string{"install": "Installing ", "update": "Updating ", "delete": "Deleting "}[verb] + name
		background(label, func(ctx context.Context) error {
			var out strings.Builder
			err := plugin.Run(ctx, h.Dirs, args, &out)
			title := map[string]string{"install": "Plugin installed", "update": "Plugin updated", "delete": "Plugin deleted"}[verb]
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
// toast, or the whole of it for a verb /plugins does not take.
func pluginsErr(verb string, err error) error {
	if !errors.Is(err, plugin.ErrUsage) {
		return err
	}
	all := strings.TrimPrefix(plugin.ErrUsage.Error(), "usage: plugins ") + " | new [what it does] | modify <name> [what to change] | toggle <name> | open <name>"
	for form := range strings.SplitSeq(all, " | ") {
		if form == verb || strings.HasPrefix(form, verb+" ") {
			return Usagef("usage: /plugins %s", form)
		}
	}
	return Usagef("usage: /plugins %s", all)
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

// openPlugins opens /plugins, or replaces the open list in place; a refresh
// after a slow action opens nothing once the user closed it, and confirm arms
// the action that runs it (Overlay.Confirm). It reads git, so it never runs
// on the key loop.
func (h *Host) openPlugins(ui UIHooks, refresh bool, confirm string) {
	ui.OpenOverlay(Overlay{
		ID: "plugins", Title: "Plugins", Origin: lua.OriginBuiltin, Refresh: refresh, Items: h.pluginItems(), Confirm: confirm,
		Reload: func() ([]any, [][]Segment) { return h.pluginItems(), nil },
	})
}

// pluginItems lists the plugins under three action rows, which carry the empty
// state, so a rebuild keeps it true.
func (h *Host) pluginItems() []any {
	ps, err := plugin.List(context.Background(), h.Dirs)
	if err != nil {
		h.Notify(slog.LevelError, "Could not read the plugins", err.Error())
	}
	install := map[string]any{"text": "Install a plugin", "verb": "fill", "fill": "/plugins install "}
	if len(ps) == 0 && err == nil {
		install["desc"] = "no plugins yet"
	}
	items := []any{map[string]any{"text": "New plugin", "verb": "fill", "fill": "/plugins new "}, install, map[string]any{"text": "Check for updates", "verb": "run", "run": "plugins check"}}
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
		if len(p.Uses) > 0 {
			desc = append(desc, "uses: "+strings.Join(p.Uses, ", "))
		}
		desc = append(desc, p.Desc)
		if loadErr != nil {
			errText = loadErr.Error()
		}
		var actions []any
		if p.Latest != "" && !strings.HasSuffix(p.Latest, "(major)") {
			label := "Update to " + p.Latest
			if strings.Contains(p.Latest, "new commit") {
				label = "Update"
			}
			actions = append(actions, map[string]any{"text": label, "hotkey": "u", "run": "plugins update " + p.Name})
		}
		if p.Enabled {
			actions = append(actions, map[string]any{"text": "Disable", "hotkey": "d", "run": "plugins disable " + p.Name})
		} else {
			actions = append(actions, map[string]any{"text": "Enable", "hotkey": "e", "run": "plugins enable " + p.Name})
		}
		actions = append(actions, map[string]any{"text": openInEditorLabel, "hotkey": "o", "run": "plugins open " + p.Name})
		if p.Local {
			actions = append(actions, map[string]any{"text": "Modify with zemi", "hotkey": "m", "fill": "/plugins modify " + p.Name + " "})
		}
		actions = append(actions, map[string]any{"text": "Delete", "hotkey": "x", "danger": true, "run": "plugins delete " + p.Name + " --yes"})
		items = append(items, map[string]any{
			"text": p.Name, "desc": strings.Join(slices.DeleteFunc(desc, func(s string) bool { return s == "" }), " · "),
			"section": section, "on": p.Enabled, "run": "plugins toggle " + p.Name,
			"detail": pluginDetail(p), "actions": actions,
			"fields": fieldList("source", source, "status", string(status), "version", version, "latest", p.Latest,
				"provides", strings.Join(provides[p.Name], " · "), "path", perm.TildePath(p.Dir), "error", errText),
		})
	}
	return items
}

// pluginGloss is what each declared name grants, for the detail pane: the same
// words as the skill's table, short enough to sit in one line of prose.
var pluginGloss = map[string]string{
	"model":           "a model or classify call",
	"agent":           "a message to the agent",
	"http":            "a fetch",
	"job":             "a command in the sandbox",
	"job.unsandboxed": "a command with the whole environment",
	"fs.write":        "a file write outside the secret and protected ones",
	"tool":            "a tool it registers being called",
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
	// only a plugin registers plugins.<its name>.<key> (zemi.config.add)
	for _, s := range h.Settings.Schema() {
		if name := settingPlugin(s.Path); name != "" {
			out[name] = append(out[name], "setting "+s.Path)
		}
	}
	return out
}
