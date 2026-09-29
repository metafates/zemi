package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
)

// configCommands is /config: every setting, edited in place.
func (h *Host) configCommands() []*Command {
	return []*Command{{
		Name: "config", NArgs: NArgsAny, Desc: "see and change every setting",
		GoFn: func(h *Host, args string) error { return h.configCommand(args) },
		GoComplete: SubcommandCompleter([]string{"set", "toggle", "reset", "pick", "lua", "open"}, func(h *Host, sub string) []string {
			if sub == "lua" || sub == "open" {
				return nil
			}
			var paths []string
			for _, s := range h.Settings.Schema() {
				paths = append(paths, s.Path)
			}
			return paths
		}),
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
	case "pick":
		return h.openConfigPick(ui, path)
	case "lua":
		ui.OpenOverlay(Overlay{ID: "config lua", Title: "Config as Lua", Origin: lua.OriginBuiltin, Body: h.configLua()})
	case "open":
		// the editor blocks and a Go command runs on the key loop
		h.GoSafe("config open", func() {
			if err := ui.EditFile(context.Background(), EditRequest{Path: filepath.Join(h.Dirs.ConfigHome, "init.lua"), Title: "init.lua"}); err != nil {
				h.Notify(logging.Error, "Could not open init.lua", err.Error())
			}
		})
	default:
		return Usagef("usage: /config [set <key> <value> | toggle <key> | reset <key> | pick <key> | lua | open]")
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

// configSet parses value in path's kind: on or off, a whole number, or the
// rest of the line as a string.
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
	case config.KindInt:
		if v, err = strconv.Atoi(value); err != nil {
			return Usagef("%s is a whole number", path)
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
		return h.SetTheme(s)
	}
	err := h.setWatched(path, val)
	if _, bad := errors.AsType[*config.ErrBadValue](err); bad {
		return usageError{err}
	}
	return err
}

// configItems lists every setting by section under two action rows. Enter
// opens a row's detail, whose actions change the value; space toggles a
// bool and passes the other rows by.
func (h *Host) configItems() []any {
	items := []any{
		map[string]any{"text": "Show as Lua", "verb": "open", "run": "config lua"},
		map[string]any{"text": "Open init.lua", "verb": "open", "run": "config open"},
	}
	for _, s := range h.Settings.Schema() {
		v, _ := h.Settings.Get(s.Path)
		source, value := h.cfgSource(s.Path), showSetting(v)+" · "
		if s.Kind == config.KindBool {
			value = "" // the switch in the key column draws it
		}
		section, _, _ := strings.Cut(s.Path, ".")
		base, set := h.cfgBase(s.Path)
		note := ""
		if set && source == "this run" {
			note = "\n\ninit.lua sets this key; the pick lasts until restart"
		}
		// The source leads every row's description, since a bool's key column
		// holds its switch; one width, so a change moves no text.
		item := map[string]any{
			"text": s.Path, "desc": fmt.Sprintf("%-8s · %s%s", source, value, s.Desc), "section": sourceSection(section),
			"fields": fieldList("kind", string(s.Kind), "default", showSetting(s.Default), "value", showSetting(v), "source", source),
			"detail": s.Desc + note,
		}
		edit := map[string]any{"text": "Edit", "hotkey": "e", "fill": fmt.Sprintf("/config set %s %v", s.Path, v)}
		switch pick := h.configPicker(s.Path); {
		case s.Kind == config.KindBool:
			item["on"], item["run"] = v == true, "config toggle "+s.Path
			edit = map[string]any{"text": "Toggle", "hotkey": "t", "run": item["run"]}
		case pick != "":
			edit = map[string]any{"text": "Change", "hotkey": "c", "run": pick}
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
	return fmt.Sprint(v)
}

// configPicker is the command that picks path's value from a list, "" for
// a key typed in.
func (h *Host) configPicker(path string) string {
	switch role, ok := strings.CutPrefix(path, "models."); {
	case ok:
		return "model " + role
	case path == "permissions.mode":
		return "mode"
	case path == "agent.profile":
		return "profile"
	case h.configChoices(path) != nil:
		return "config pick " + path
	}
	return ""
}

// configChoices is what /config pick offers for path; "" is the default.
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

// openConfigPick lists path's choices; closing it rebuilds /config under it.
func (h *Host) openConfigPick(ui UIHooks, path string) error {
	choices := h.configChoices(path)
	if choices == nil {
		return Usagef("%s has no list to pick from; /config set %s <value>", path, path)
	}
	cur, sel := h.Settings.Str(path), 0
	var items []any
	for i, c := range choices {
		item := map[string]any{"text": cmp.Or(c, "default"), "run": "config set " + path + " " + c}
		if c == cur {
			item["key"], sel = "current", i
		}
		items = append(items, item)
	}
	ui.OpenOverlay(Overlay{ID: "config pick", Title: path, Origin: lua.OriginBuiltin, Items: items, Selected: sel, Verb: "pick"})
	return nil
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

// luaLiteral writes a string with only the escapes Lua 5.1 reads; other
// bytes, UTF-8 included, stay as they are.
func luaLiteral(v any) string {
	s, ok := v.(string)
	if !ok {
		return fmt.Sprint(v)
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range []byte(s) {
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < ' ' || c == 0x7f:
			fmt.Fprintf(&b, `\%03d`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
