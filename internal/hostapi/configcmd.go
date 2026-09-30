package hostapi

import (
	"cmp"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
)

// configCommands is /config: every setting, edited in place.
func (h *Host) configCommands() []*Command {
	return []*Command{{
		Name: "config", NArgs: NArgsAny, Desc: "see and change every setting",
		GoFn: func(h *Host, args string) error { return h.configCommand(args) },
		GoComplete: SubcommandCompleter([]string{"set", "toggle", "reset", "edit", "lua", "open"}, func(h *Host, sub string) []string {
			if sub == "lua" || sub == "open" {
				return nil
			}
			var paths []string
			for _, s := range h.Settings.Schema() {
				paths = append(paths, s.Path)
			}
			return paths
		}),
	}, {
		Name: "theme", NArgs: NArgsOptional, Desc: "pick the theme; the screen repaints in it as you pick",
		GoFn: func(h *Host, args string) error {
			if args = strings.TrimSpace(args); args != "" {
				return h.SetConfig("ui.theme", args)
			}
			ui, err := h.Screen()
			if err != nil {
				return err
			}
			return h.openConfigEdit(ui, "ui.theme")
		},
		GoComplete: func(h *Host, _, _ string) []string {
			var names []string
			for _, t := range h.Themes.List() {
				names = append(names, t.Name)
			}
			return names
		},
	}}
}

func (h *Host) configCommand(args string) error {
	verb, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	path, value, _ := strings.Cut(strings.TrimSpace(rest), " ")
	if verb == "set" {
		return h.configSet(path, strings.TrimSpace(value))
	}
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	switch verb {
	case "":
		o := Overlay{
			ID: "config", Title: "Config", Origin: lua.OriginBuiltin, Items: h.configItems(),
			Reload: func() ([]any, [][]Segment) { return h.configItems(), nil },
			// what init.lua sets past its error never ran
			Fields: []Field{{"error", h.InitError()}},
		}
		ui.OpenOverlay(o)
	case "toggle", "reset":
		s, err := h.setting(path)
		if err != nil {
			return err
		}
		if verb == "reset" {
			base, _ := h.cfgBase(path)
			return h.SetConfig(path, base)
		}
		if s.Kind != config.KindBool {
			return Usagef("%s is not on or off; /config set %s <value>", path, path)
		}
		return h.SetConfig(path, !h.Settings.Bool(path))
	case "edit":
		return h.openConfigEdit(ui, path)
	case "lua":
		ui.OpenOverlay(Overlay{ID: "config lua", Title: "Config as Lua", Origin: lua.OriginBuiltin, Body: h.configLua()})
	case "open":
		h.openInEditor(ui, filepath.Join(h.Dirs.ConfigHome, "init.lua"), "init.lua")
	default:
		return Usagef("usage: /config [set <key> <value> | toggle <key> | reset <key> | edit <key> | lua | open]")
	}
	return nil
}

func (h *Host) setting(path string) (config.Setting, error) {
	all := h.Settings.Schema()
	if i := slices.IndexFunc(all, func(s config.Setting) bool { return s.Path == path }); i >= 0 {
		return all[i], nil
	}
	return config.Setting{}, Usagef("no setting is named %q; /config lists them", path)
}

// configSet parses value in path's kind: on or off, a number, or the rest
// of the line as a string.
func (h *Host) configSet(path, value string) error {
	s, err := h.setting(path)
	if err != nil {
		return err
	}
	var v any = value
	switch s.Kind {
	case config.KindBool:
		b, ok := map[string]bool{"on": true, "true": true, "off": false, "false": false}[value]
		if !ok {
			return Usagef("%s is on or off", path)
		}
		v = b
	case config.KindNumber:
		if v, err = strconv.ParseFloat(value, 64); err != nil {
			return Usagef("%s is a number", path)
		}
	case config.KindString:
	}
	return h.SetConfig(path, v)
}

// SetConfig writes a config key at runtime (/config, /effort): a key with a
// setter of its own goes through it, which validates, and every write
// fires the key's watches.
func (h *Host) SetConfig(path string, val any) error {
	s, _ := val.(string)
	if role, ok := strings.CutPrefix(path, "models."); ok {
		return h.SetModel(role+" "+cmp.Or(s, "chat"), true)
	}
	switch path {
	case "permissions.mode":
		return h.SetPermMode(s)
	case "agent.profile":
		return h.SetProfile(s)
	case "ui.theme":
		if err := h.checkTheme(s); err != nil {
			return err
		}
	}
	err := h.setWatched(path, val)
	if _, bad := errors.AsType[*config.BadValueError](err); bad {
		return usageError{err}
	}
	return err
}

// configItems lists every setting by section under two action rows. Enter
// opens a row's detail, whose actions change the value; space toggles a
// bool and passes the other rows by.
func (h *Host) configItems() []any {
	items := []any{
		map[string]any{"text": "Show as Lua", "verb": "open", "run": "config lua", "preview": "every setting off its default, as the zemi.config.set lines init.lua would write"},
		map[string]any{"text": "Open init.lua", "verb": "open", "run": "config open", "preview": "your config in the editor; zemi reloads when you save it"},
	}
	for _, s := range h.Settings.Schema() {
		v, _ := h.Settings.Get(s.Path)
		source := h.cfgSource(s.Path)
		desc := fmt.Sprintf("%-8s  %s", source, showSetting(v))
		if s.Kind == config.KindBool {
			desc = source // the switch in the key column draws the value
		}
		section, _, _ := strings.Cut(s.Path, ".")
		section = sourceSection(section)
		if name := settingPlugin(s.Path); name != "" {
			section = "Plugin " + name
		}
		base, set := h.cfgBase(s.Path)
		note := ""
		if set && source == "this run" {
			note = "\n\ninit.lua sets this key; the pick lasts until restart"
		}
		// The source leads every row's description, since a bool's key column
		// holds its switch; one width, so a change moves no text. The key's
		// description is the preview, whole under the list.
		item := map[string]any{
			"text": s.Path, "desc": desc, "preview": s.Desc, "section": section,
			"fields": fieldList("kind", string(s.Kind), "default", showSetting(s.Default), "value", showSetting(v), "source", source),
			"detail": s.Desc + note,
		}
		edit := map[string]any{"text": "Change", "hotkey": "c", "run": configPicker(s.Path)}
		if s.Kind == config.KindBool {
			item["on"], item["run"] = v == true, "config toggle "+s.Path
			edit = map[string]any{"text": "Toggle", "hotkey": "t", "run": item["run"]}
		}
		if item["run"] == nil {
			item["verb"] = "open" // a row with a verb is no toggle
		}
		actions := []any{edit}
		if !reflect.DeepEqual(v, base) {
			label := map[bool]string{true: "Reset to init.lua", false: "Reset to default"}[set]
			actions = append(actions, map[string]any{"text": label, "hotkey": "r", "run": "config reset " + s.Path})
		}
		if note != "" {
			actions = append(actions, map[string]any{"text": "Open init.lua", "hotkey": "o", "run": "config open"})
		}
		item["actions"] = actions
		items = append(items, item)
	}
	return items
}

func showSetting(v any) string {
	switch v := v.(type) {
	case bool:
		return map[bool]string{true: "on", false: "off"}[v]
	case string:
		return cmp.Or(v, "empty")
	}
	return luaLiteral(v)
}

// configPicker is the command that changes path's value on a screen of its
// own: the key's picker, else /config edit.
func configPicker(path string) string {
	switch role, ok := strings.CutPrefix(path, "models."); {
	case ok:
		return "model " + role
	case path == "permissions.mode":
		return "mode"
	case path == "agent.profile":
		return "profile"
	case path == "ui.theme":
		return "theme"
	}
	return "config edit " + path
}

// configChoices is what /config edit offers for path; "" is the default.
func (h *Host) configChoices(path string) []string {
	switch path {
	case "ui.theme":
		var names []string
		for _, t := range h.Themes.List() {
			// a light variant draws by itself on a light terminal
			if base, ok := strings.CutSuffix(t.Name, "-light"); !ok || h.Themes.Get(base) == nil {
				names = append(names, t.Name)
			}
		}
		return names
	}
	s, _ := h.setting(path)
	return s.Choices
}

// openConfigEdit prompts for path's value, or lists its choices, where
// enter sets one and keeps the list open, so a theme repaints the screen,
// the list's own box included, as its preview. Closing either rebuilds
// /config under it.
func (h *Host) openConfigEdit(ui UIHooks, path string) error {
	s, err := h.setting(path)
	if err != nil {
		return err
	}
	switch {
	case s.Kind == config.KindBool:
		return Usagef("%s is on or off; /config toggle %s", path, path)
	case h.configChoices(path) == nil:
		v, _ := h.Settings.Get(path)
		text, ok := v.(string)
		if !ok {
			text = luaLiteral(v) // a number as /config set reads it
		}
		ui.OpenOverlay(Overlay{ID: "config edit", Title: path, Origin: lua.OriginBuiltin, Input: true, Default: text, Run: "config set " + path})
		return nil
	}
	items, sel := h.pickItems(path)
	ui.OpenOverlay(Overlay{
		ID: "config edit", Title: path, Origin: lua.OriginBuiltin, Items: items, Selected: sel,
		Reload: func() ([]any, [][]Segment) { items, _ := h.pickItems(path); return items, nil },
	})
	return nil
}

// pickItems are path's choices as rows, the current one keyed and its index.
func (h *Host) pickItems(path string) (items []any, sel int) {
	cur := h.Settings.Str(path)
	for i, c := range h.configChoices(path) {
		item := map[string]any{"text": cmp.Or(c, "default"), "run": "config set " + path + " " + c, "verb": "pick"}
		if c == cur {
			item["key"], sel = "current", i
		}
		if path == "ui.theme" && h.Themes.Get(c+"-light") != nil {
			item["desc"] = "light: " + c + "-light"
		}
		items = append(items, item)
	}
	return items, sel
}

// configLua is every setting off its default as the line init.lua sets it
// with, for y to copy.
func (h *Host) configLua() string {
	var lines []string
	for _, s := range h.Settings.Schema() {
		if v, _ := h.Settings.Get(s.Path); !reflect.DeepEqual(v, s.Default) {
			lines = append(lines, fmt.Sprintf("zemi.config.set(%q, %s)", s.Path, luaLiteral(v)))
		}
	}
	if lines == nil {
		return "-- every setting is at its default"
	}
	return strings.Join(lines, "\n")
}
