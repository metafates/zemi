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

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
)

// configCommands is /config: every setting, edited in place.
func (h *Host) configCommands() []*Command {
	return []*Command{{
		Name: "config", NArgs: NArgsAny, Desc: "see and change every setting",
		GoFn: func(h *Host, args string, confirm bool) error { return h.configCommand(args) },
		GoComplete: SubcommandCompleter([]string{"set", "toggle", "reset", "edit", "lua", "open"}, func(h *Host, sub string) []string {
			if sub == "lua" || sub == "open" {
				return nil
			}
			var paths []string
			for _, s := range h.Settings.Schema() {
				paths = append(paths, string(s.Path))
			}
			return paths
		}),
	}, {
		Name: "theme", NArgs: NArgsOptional, Desc: "pick the theme; the screen repaints in it as you pick",
		GoFn: func(h *Host, args string, confirm bool) error {
			if args = strings.TrimSpace(args); args != "" {
				return h.SetConfig(config.KeyUITheme, args)
			}
			ui, err := h.Screen()
			if err != nil {
				return err
			}
			return h.openConfigEdit(ui, config.KeyUITheme)
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
	key := config.Key(path)
	if verb == "set" {
		return h.configSet(key, strings.TrimSpace(value))
	}
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	switch verb {
	case "":
		o := Overlay{
			ID: "config", Title: "Config", Origin: lua.OriginBuiltin, Items: h.configSections(),
			Reload: h.configSections,
			// what init.lua sets past its error never ran
			Fields: []Field{{"error", h.InitError()}},
		}
		ui.OpenOverlay(o)
	case "toggle", "reset":
		s, err := h.setting(key)
		if err != nil {
			return err
		}
		if verb == "reset" {
			base, _ := h.cfgBase(key)
			return h.SetConfig(key, base)
		}
		if s.Kind != config.KindBool {
			return Usagef("%s is not on or off; /config set %s <value>", path, path)
		}
		return h.SetConfig(key, !h.Settings.Bool(key))
	case "edit":
		return h.openConfigEdit(ui, key)
	case "lua":
		ui.OpenOverlay(Overlay{ID: "config lua", Title: "Config as Lua", Origin: lua.OriginBuiltin, Body: h.configLua()})
	case "open":
		h.openInEditor(ui, filepath.Join(h.Dirs.ConfigHome, "init.lua"), "init.lua")
	default:
		if h.configSection(verb) { // /config <section> opens the section's keys
			ui.OpenOverlay(Overlay{
				ID: "config " + verb, Title: "Config / " + sourceSection(verb), Origin: lua.OriginBuiltin,
				Items: h.configItems(verb), Reload: func() []OverlayItem { return h.configItems(verb) },
			})
			return nil
		}
		return Usagef("usage: /config [set <key> <value> | toggle <key> | reset <key> | edit <key> | lua | open | <section>]")
	}
	return nil
}

// configSection reports whether name is a first path segment of the schema,
// the sections /config falls into.
func (h *Host) configSection(name string) bool {
	return slices.ContainsFunc(h.Settings.Schema(), func(s config.Setting) bool {
		seg, _, _ := strings.Cut(string(s.Path), ".")
		return seg == name
	})
}

// configSections is /config's root: one row per section of the schema, in
// schema order, over the two actions. The preview names the keys, so the
// reader knows what falls where before falling in.
func (h *Host) configSections() []OverlayItem {
	counts := map[string]int{}
	var order []string
	keys := map[string][]string{}
	for _, s := range h.Settings.Schema() {
		seg, rest, _ := strings.Cut(string(s.Path), ".")
		if counts[seg]++; counts[seg] == 1 {
			order = append(order, seg)
		}
		keys[seg] = append(keys[seg], rest)
	}
	items := []OverlayItem{
		{Text: "Show as Lua", Verb: VerbOpen, Run: "config lua", Preview: "every setting off its default, as the zemi.config.set lines init.lua would write"},
		{Text: "Open init.lua", Verb: VerbOpen, Run: "config open", Preview: "your config in the editor; zemi reloads when you save it"},
	}
	for _, sec := range order {
		desc, ok := sectionDesc[sec]
		if !ok {
			desc = pluralCount(counts[sec], "setting", "settings")
		}
		items = append(items, OverlayItem{
			Text: sourceSection(sec), Desc: desc, Preview: strings.Join(keys[sec], ", "),
			Verb: VerbOpen, Run: "config " + sec,
		})
	}
	return items
}

// sectionDesc is the line a /config section row reads by, per first path
// segment of the schema.
var sectionDesc = map[string]string{
	"models":    "the model each job runs on",
	"providers": "the builtin providers and their keys",
	"ui":        "the theme, the mouse and alerts",
	"agent":     "reasoning effort and background agents",
	"session":   "the idle recap",
	"lsp":       "language servers and when they start",
	"editor":    "the editor zemi opens",
	"keymap":    "the key <leader> stands for",
	"log":       "what the launch log records",
	"plugins":   "settings the plugins register",
}

func (h *Host) setting(path config.Key) (config.Setting, error) {
	all := h.Settings.Schema()
	if i := slices.IndexFunc(all, func(s config.Setting) bool { return s.Path == path }); i >= 0 {
		return all[i], nil
	}
	return config.Setting{}, Usagef("no setting is named %q; /config lists them", path)
}

// configSet parses value in path's kind: on or off, a number, or the rest
// of the line as a string.
func (h *Host) configSet(path config.Key, value string) error {
	s, err := h.setting(path)
	if err != nil {
		return err
	}
	var v any = value
	switch s.Kind {
	case config.KindList:
		v = listValue(value)
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
	case config.KindString, config.KindDuration: // the line is the value; checkKind judges it
	}
	return h.SetConfig(path, v)
}

// SetConfig writes a config key at runtime (/config, /effort): a key with a
// setter of its own goes through it, which validates, and every write
// fires the key's watches.
func (h *Host) SetConfig(path config.Key, val any) error {
	s, _ := val.(string)
	if role, ok := config.ModelRole(path); ok {
		return h.SetTargetModel(role, cmp.Or(s, defaultRole))
	}
	//nolint:exhaustive // a model target's key is no builtin constant
	switch path {
	case config.KeyUITheme:
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

// configItems lists one section's settings with the source leading every
// row's description. Enter opens a row's detail, whose actions change the
// value; space toggles a bool and passes the other rows by.
func (h *Host) configItems(section string) []OverlayItem {
	var items []OverlayItem
	for _, s := range h.Settings.Schema() {
		seg, _, _ := strings.Cut(string(s.Path), ".")
		if seg != section {
			continue
		}
		v, _ := h.Settings.Get(s.Path)
		source := h.cfgSource(s.Path)
		desc := fmt.Sprintf("%-8s  %s", source, showSetting(v))
		if s.Kind == config.KindBool {
			desc = source // the switch in the key column draws the value
		}
		listSection := ""
		if name := settingPlugin(s.Path); name != "" {
			listSection = "Plugin " + name
		}
		base, set := h.cfgBase(s.Path)
		note := ""
		if set && source == "this run" {
			note = "\n\ninit.lua sets this key; the pick lasts until restart"
		}
		// The source leads every row's description, since a bool's key column
		// holds its switch; one width, so a change moves no text. The key's
		// description is the preview, whole under the list.
		item := OverlayItem{
			Text: string(s.Path), Desc: desc, Preview: s.Desc, Section: listSection,
			Fields: fieldList("kind", string(s.Kind), "default", showSetting(s.Default), "value", showSetting(v), "source", source),
			Detail: s.Desc + note,
		}
		edit := ItemKey{Key: "c", Desc: "change", Run: configPicker(s.Path)}
		if s.Kind == config.KindBool {
			item.On, item.Run = new(v == true), "config toggle "+string(s.Path)
			edit = ItemKey{Key: "t", Desc: "toggle", Run: item.Run}
		}
		if item.Run == "" {
			item.Verb = VerbOpen // a row with a verb is no toggle
		}
		keys := []ItemKey{edit}
		if !reflect.DeepEqual(v, base) {
			label := map[bool]string{true: "reset to init.lua", false: "reset to default"}[set]
			keys = append(keys, ItemKey{Key: "r", Desc: label, Run: "config reset " + string(s.Path)})
		}
		if note != "" {
			keys = append(keys, ItemKey{Key: "o", Desc: "open init.lua", Run: "config open"})
		}
		item.Keys = keys
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
	case []string:
		return cmp.Or(strings.Join(v, ", "), "none")
	}
	return luaLiteral(v)
}

// listValue is a list key's line: entries separated by commas or spaces.
func listValue(value string) []string {
	out := []string{}
	for _, f := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, strings.TrimSpace(f))
	}
	return out
}

// configPicker is the command that changes path's value on a screen of its
// own: the key's picker, else /config edit.
func configPicker(path config.Key) string {
	switch role, ok := config.ModelRole(path); {
	case ok:
		return "model " + role
	case path == config.KeyUITheme:
		return "theme"
	}
	return "config edit " + string(path)
}

// settingGhost is what an empty input means for the setting, shown dim in
// the prompt: a bare enter sets exactly that.
func settingGhost(s config.Setting) string {
	//nolint:exhaustive // a bool edits on a switch, a list has no prompt
	switch s.Kind {
	case config.KindDuration:
		if d, _ := s.Default.(string); d != "" {
			return "default " + d
		}
		return "empty = off"
	case config.KindNumber:
		return "default " + showSetting(s.Default)
	case config.KindString:
		if d, _ := s.Default.(string); d != "" {
			return "default " + d
		}
	}
	return ""
}

// configChoices is what /config edit offers for path; "" is the default.
func (h *Host) configChoices(path config.Key) []string {
	//nolint:exhaustive // a model target's key is no builtin constant
	switch path {
	case config.KeyUITheme:
		var names []string
		for _, t := range h.Themes.List() {
			names = append(names, t.Name)
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
func (h *Host) openConfigEdit(ui UIHooks, path config.Key) error {
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
			if list, isList := v.([]string); isList {
				// NOTE: the list grammar splits on comma and space, so an entry
				// holding either cannot round-trip; same ceiling as configSet.
				text = strings.Join(list, ", ")
			} else {
				text = luaLiteral(v) // a number as /config set reads it
			}
		}
		ui.OpenOverlay(Overlay{ID: "config edit", Title: string(path), Origin: lua.OriginBuiltin, Input: true, Default: text, Placeholder: settingGhost(s), Run: "config set " + string(path)})
		return nil
	}
	items, sel := h.pickItems(path)
	title := string(path)
	if path == config.KeyUITheme { // /theme opens it
		title = "Theme"
	}
	ui.OpenOverlay(Overlay{
		ID: "config edit", Title: title, Origin: lua.OriginBuiltin, Items: items, Selected: sel,
		Reload: func() []OverlayItem { items, _ := h.pickItems(path); return items },
	})
	return nil
}

// pickItems are path's choices as rows, the current one keyed and its index.
func (h *Host) pickItems(path config.Key) (items []OverlayItem, sel int) {
	cur := h.Settings.Str(path)
	for i, c := range h.configChoices(path) {
		item := OverlayItem{Text: cmp.Or(c, "default"), Run: "config set " + string(path) + " " + c, Verb: VerbPick}
		// a reasoning level reads by its display name ("X-High"), the value
		// the config stores beside it
		if path == config.KeyAgentEffort && c != "" && c != cur {
			item.Desc = agent.EffortName(c)
		}
		if c == cur {
			item.Tag, sel = "current", i
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
