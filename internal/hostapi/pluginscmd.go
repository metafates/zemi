package hostapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/plugins"
)

// newPluginPrompt rides behind /plugins new and modifyPluginPrompt behind
// /plugins modify; the procedure lives in the zemi skill.
const (
	newPluginPrompt    = "I want a new zemi plugin that does what the command above says; ask me when it says nothing. Load the zemi skill and follow the Plugin steps in its plugins.md: tests, not this session, are the check."
	modifyPluginPrompt = "Change my zemi plugin %s in %s as the command above says; ask me when it says nothing. Load the zemi skill and follow the Plugin steps in its plugins.md: tests, not this session, are the check."
)

// pluginsCommands is /plugins: the zemi plugins verbs plus new, modify,
// toggle and open, which need the screen.
func (h *Host) pluginsCommands() []*Command {
	return []*Command{{
		Name: "plugins", NArgs: NArgsAny, Desc: "install, update and turn plugins on or off",
		GoFn: (*Host).pluginsCommand,
		GoComplete: SubcommandCompleter([]string{"new", "modify", "install", "update", "check", "enable", "disable", "delete", "toggle", "open"}, func(h *Host, sub string) []string {
			if sub == "new" || sub == "install" || sub == "check" {
				return nil
			}
			ps, _ := plugins.Scan(h.Dirs)
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
	ui, err := h.Screen()
	if err != nil {
		return fmt.Errorf("%w; zemi plugins manages them from the shell", err)
	}
	typed, stdin := agent.CutAttached(line)
	args, verb := strings.Fields(typed), ""
	if len(args) > 0 {
		verb = args[0]
	}
	send := func(prompt string) {
		msg := "/plugins " + strings.TrimSpace(typed) + agent.Attached("command", prompt) + stdin
		h.GoSafe("plugins "+verb, func() {
			if err := h.AgentSend(msg); err != nil {
				h.Notify(logging.Error, "Could not send", h.friendlyError(err))
			}
		})
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
				h.CommandFailed("plugins", pluginsErr(err))
				if errors.Is(err, plugins.ErrUsage) {
					return
				}
			}
			h.openPlugins(ui, true)
		})
	}
	switch {
	case len(args) == 0 || verb == "list" && len(args) == 1:
		h.GoSafe("plugins", func() { h.openPlugins(ui, false) })
	case verb == "toggle" && len(args) == 2:
		p, err := h.pluginNamed(args[1])
		if err != nil {
			return err
		}
		return pluginsErr(plugins.SetEnabled(h.Dirs, p.Name, !p.Enabled))
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
		// the editor blocks and a Go command runs on the key loop
		h.GoSafe("plugins open", func() {
			if err := ui.EditFile(context.Background(), EditRequest{Path: path, Title: "the plugin"}); err != nil {
				h.Notify(logging.Error, "Could not open the plugin", err.Error())
			}
		})
	case verb == "new":
		send(newPluginPrompt)
	case verb == "modify" && len(args) >= 2:
		p, err := h.pluginNamed(args[1])
		if err != nil {
			return err
		}
		if !p.Local {
			return Usagef("%s is an installed plugin, and the agent changes only local ones: delete it and ask the agent to clone it into the local plugins directory", p.Name)
		}
		// the user asked for the change, as for a plugin whose creation they
		// allowed: auto mode settles the agent's edits there until zemi exits
		h.madePlugins.Store(h.localPluginDir(p.Dir), true)
		send(fmt.Sprintf(modifyPluginPrompt, p.Name, TildePath(p.Dir)))
	case verb == "enable" || verb == "disable":
		// at once, so the list an action returns to shows it
		return pluginsErr(plugins.Run(context.Background(), h.Dirs, args, io.Discard))
	case verb == "check" && len(args) == 1:
		background("Checking plugins", func(ctx context.Context) error {
			ps, err := plugins.Check(ctx, h.Dirs)
			var names []string
			for _, p := range ps {
				if p.Latest != "" {
					names = append(names, p.Name)
				}
			}
			if len(names) > 0 {
				h.Notify(logging.Info, "Updates available", strings.Join(names, ", "))
			} else if err == nil {
				h.Notify(logging.Info, plugins.UpToDate, "")
			}
			return err
		})
	case verb == "install" || verb == "update" || verb == "delete":
		// install takes a URL and an optional name, the last word either way
		name := "plugins"
		if len(args) > 1 {
			name = plugins.Name(args[len(args)-1])
		}
		label := map[string]string{"install": "Installing ", "update": "Updating ", "delete": "Deleting "}[verb] + name
		background(label, func(ctx context.Context) error {
			var out strings.Builder
			err := plugins.Run(ctx, h.Dirs, args, &out)
			title := map[string]string{"install": "Plugin installed", "update": "Plugin updated", "delete": "Plugin deleted"}[verb]
			body := strings.TrimSpace(out.String())
			if body == plugins.UpToDate {
				title, body = body, ""
			} else if verb == "delete" && err == nil {
				body = args[1]
			}
			if err == nil || body != "" {
				h.Notify(logging.Info, title, body)
			}
			return err
		})
	default:
		return pluginsErr(plugins.ErrUsage)
	}
	return nil
}

func pluginsErr(err error) error {
	if errors.Is(err, plugins.ErrUsage) {
		return Usagef("%v | new [what it does] | modify <name> [what to change] | toggle <name> | open <name> (TUI only)", err)
	}
	return err
}

// pluginNamed finds a plugin by name, a local one first.
func (h *Host) pluginNamed(name string) (plugins.Plugin, error) {
	ps, err := plugins.Scan(h.Dirs)
	if err != nil {
		return plugins.Plugin{}, err
	}
	if i := slices.IndexFunc(ps, func(p plugins.Plugin) bool { return p.Name == name }); i >= 0 {
		return ps[i], nil
	}
	return plugins.Plugin{}, Usagef("no plugin is named %q", name)
}

// openPlugins opens /plugins, or replaces the open list in place; a refresh
// after a slow action opens nothing once the user closed it. It reads git, so
// it never runs on the key loop.
func (h *Host) openPlugins(ui UIHooks, refresh bool) {
	ui.OpenOverlay(Overlay{
		ID: "plugins", Title: "Plugins", Origin: lua.OriginBuiltin, Fields: true, Refresh: refresh, Items: h.pluginItems(),
		Reload: func() ([]any, [][]Segment) { return h.pluginItems(), nil },
	})
}

// pluginItems lists the plugins under three action rows, which carry the empty
// state, so a rebuild keeps it true.
func (h *Host) pluginItems() []any {
	ps, err := plugins.List(context.Background(), h.Dirs)
	if err != nil {
		h.Notify(logging.Error, "Could not read the plugins", err.Error())
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
		desc = append(desc, p.Version, p.Desc)
		source, section := p.URL, "Installed"
		if p.Local {
			source, section = "local", "Local"
		}
		detail := []string{"source: " + source}
		if !p.Local {
			detail = append(detail, "version: "+p.Version)
		}
		if p.Latest != "" {
			detail = append(detail, "latest: "+p.Latest)
		}
		detail = append(detail, "path: "+TildePath(p.Dir), "status: "+string(status))
		if l := provides[p.Name]; l != nil {
			detail = append(detail, "provides: "+strings.Join(l, " · "))
		}
		if loadErr != nil { // last: every line after it is its text
			detail = append(detail, "error: "+loadErr.Error())
		}
		if p.Desc != "" {
			detail = append(detail, "", p.Desc)
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
		actions = append(actions, map[string]any{"text": "Open in the editor", "hotkey": "o", "run": "plugins open " + p.Name})
		if p.Local {
			actions = append(actions, map[string]any{"text": "Modify with the agent", "hotkey": "m", "fill": "/plugins modify " + p.Name + " "})
		}
		actions = append(actions, map[string]any{"text": "Delete", "hotkey": "x", "hl": "Danger", "run": "plugins delete " + p.Name})
		items = append(items, map[string]any{
			"text": p.Name, "desc": strings.Join(slices.DeleteFunc(desc, func(s string) bool { return s == "" }), " · "),
			"section": section, "on": p.Enabled, "run": "plugins toggle " + p.Name,
			"detail": strings.Join(detail, "\n"), "actions": actions,
		})
	}
	return items
}

// pluginStatus is how the last boot left p: "starting" when it is on but no
// boot has run it yet.
func (h *Host) pluginStatus(p plugins.Plugin) (ServiceStatus, error) {
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
	return out
}
