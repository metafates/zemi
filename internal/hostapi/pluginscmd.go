package hostapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/plugins"
)

// pluginsCommands is /plugins: the zemi plugins verbs plus toggle and open,
// which need the screen.
func (h *Host) pluginsCommands() []*Command {
	return []*Command{{
		Name: "plugins", NArgs: "*", Desc: "install, update and turn plugins on or off",
		GoFn: func(h *Host, args string) error { return h.pluginsCommand(strings.Fields(args)) },
		GoComplete: subcommandCompleter([]string{"install", "update", "check", "enable", "disable", "delete", "toggle", "open"}, func(h *Host, sub string) []string {
			if sub == "install" || sub == "check" {
				return nil
			}
			ps, _ := plugins.Scan(h.Dirs)
			var names []string
			for _, p := range ps {
				names = append(names, p.Name)
			}
			return names
		}),
	}}
}

func (h *Host) pluginsCommand(args []string) error {
	ui, err := h.Screen()
	if err != nil {
		return fmt.Errorf("%w; zemi plugins manages them from the shell", err)
	}
	verb := ""
	if len(args) > 0 {
		verb = args[0]
	}
	// git, or a big RemoveAll: off the key loop, then the list again
	background := func(fn func() error) {
		h.GoSafe("plugins "+verb, func() {
			if err := fn(); err != nil {
				h.CommandFailed("plugins", pluginsErr(err))
				if errors.Is(err, plugins.ErrUsage) {
					return
				}
			}
			h.openPlugins(ui)
		})
	}
	switch {
	case len(args) == 0 || verb == "list" && len(args) == 1:
		h.GoSafe("plugins", func() { h.openPlugins(ui) })
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
		// the editor blocks and a Go command runs on the key loop
		h.GoSafe("plugins open", func() {
			if err := ui.EditFile(context.Background(), EditRequest{Path: filepath.Join(p.Dir, "init.lua"), Title: "the plugin"}); err != nil {
				h.Notify("error", "Could not open the plugin", err.Error())
			}
		})
	case verb == "enable" || verb == "disable":
		// at once, so the list an action returns to shows it
		return pluginsErr(plugins.Run(context.Background(), h.Dirs, args, io.Discard))
	case verb == "check" && len(args) == 1:
		background(func() error {
			ps, err := plugins.Check(context.Background(), h.Dirs)
			var names []string
			for _, p := range ps {
				if p.Latest != "" {
					names = append(names, p.Name)
				}
			}
			if len(names) > 0 {
				h.Notify("info", "Updates available", strings.Join(names, ", "))
			} else if err == nil {
				h.Notify("info", "Plugins up to date", "")
			}
			return err
		})
	case verb == "install" || verb == "update" || verb == "delete":
		background(func() error {
			var out strings.Builder
			err := plugins.Run(context.Background(), h.Dirs, args, &out)
			if body := strings.TrimSpace(out.String()); body != "" {
				h.Notify("info", map[string]string{"install": "Plugin installed", "update": "Plugin updated"}[verb], body)
			} else if err == nil && verb == "delete" {
				h.Notify("info", "Plugin deleted", args[1])
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
		return Usagef("%v | toggle <name> | open <name> (TUI only)", err)
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

// openPlugins opens /plugins, or replaces the open list in place. It reads
// git, so it never runs on the key loop.
func (h *Host) openPlugins(ui UIHooks) {
	ui.OpenOverlay(Overlay{ID: "plugins", Title: "Plugins", Origin: lua.OriginBuiltin, Items: h.pluginItems(),
		Reload: func() ([]any, [][]Segment) { return h.pluginItems(), nil }})
}

// pluginItems lists the plugins under two action rows, which carry the empty
// state, so a rebuild keeps it true.
func (h *Host) pluginItems() []any {
	ps, err := plugins.List(context.Background(), h.Dirs)
	if err != nil {
		h.Notify("error", "Could not read the plugins", err.Error())
	}
	install := map[string]any{"text": "Install a plugin", "verb": "install", "fill": "/plugins install "}
	if len(ps) == 0 && err == nil {
		install["desc"] = "No plugins yet."
	}
	items := []any{install, map[string]any{"text": "Check for updates", "verb": "check", "run": "plugins check"}}
	for _, p := range ps {
		status, loadErr := h.pluginStatus(p)
		var desc []string
		if status == "failed" {
			desc = append(desc, status)
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
		detail = append(detail, "path: "+TildePath(p.Dir), "status: "+status)
		if loadErr != nil {
			detail = append(detail, "error: "+loadErr.Error())
		}
		if provides := h.pluginProvides(p.Name); provides != "" {
			detail = append(detail, "provides: "+provides)
		}
		if p.Desc != "" {
			detail = append([]string{p.Desc, ""}, detail...)
		}
		var actions []any
		if p.Latest != "" && !strings.HasSuffix(p.Latest, "(major)") {
			label := "Update to " + p.Latest
			if strings.Contains(p.Latest, "new commit") {
				label = "Update"
			}
			actions = append(actions, map[string]any{"text": label, "hotkey": "u", "run": "plugins update " + p.Name})
		}
		key := "on"
		if p.Enabled {
			actions = append(actions, map[string]any{"text": "Disable", "hotkey": "d", "run": "plugins disable " + p.Name})
		} else {
			key = "off"
			actions = append(actions, map[string]any{"text": "Enable", "hotkey": "e", "run": "plugins enable " + p.Name})
		}
		actions = append(actions, map[string]any{"text": "Open in the editor", "hotkey": "o", "run": "plugins open " + p.Name})
		if !p.Local {
			actions = append(actions, map[string]any{"text": "Delete", "hotkey": "x", "hl": "Danger", "run": "plugins delete " + p.Name})
		}
		items = append(items, map[string]any{
			"text": p.Name, "desc": strings.Join(slices.DeleteFunc(desc, func(s string) bool { return s == "" }), " · "),
			"section": section, "key": key, "run": "plugins toggle " + p.Name,
			"detail": strings.Join(detail, "\n"), "actions": actions,
		})
	}
	return items
}

// pluginStatus is how the last boot left p: "starting" when it is on but no
// boot has run it yet.
func (h *Host) pluginStatus(p plugins.Plugin) (string, error) {
	if !p.Enabled {
		return "off", nil
	}
	if loads := h.pluginLoads.Load(); loads != nil {
		for _, l := range *loads {
			if l.dir != p.Dir {
				continue
			}
			if l.err != nil {
				return "failed", l.err
			}
			return "ready", nil
		}
	}
	return "starting", nil
}

// pluginProvides names what plugin name registered in this generation.
func (h *Host) pluginProvides(name string) string {
	var out []string
	mine := func(o lua.Origin, path string) bool { return o == lua.OriginPlugin && h.PluginOf(path) == name }
	for _, c := range h.Commands.List() {
		if mine(c.Origin, c.Path) {
			out = append(out, "command /"+c.Name)
		}
	}
	for _, t := range h.luaTools.list() {
		if mine(t.Origin, t.Path) {
			out = append(out, "tool "+t.Name)
		}
	}
	for _, w := range h.Widgets.List() {
		if mine(w.Origin, w.Path) {
			out = append(out, "widget "+w.ID)
		}
	}
	for _, k := range h.Keymaps.List() {
		if mine(k.Origin, k.Path) {
			out = append(out, "keymap "+k.Key)
		}
	}
	return strings.Join(out, " · ")
}
