package hostapi

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
)

// cfgBlockStart and cfgBlockEnd mark the lines /config save appends at the
// end of init.lua, where they run last and so win.
const (
	cfgBlockStart = "-- zemi: saved from /config"
	cfgBlockEnd   = "-- zemi: end"
)

// configCommands is /config: every setting, edited in place, and the
// runtime picks written back into init.lua.
func (h *Host) configCommands() []*Command {
	return []*Command{{
		Name: "config", NArgs: NArgsAny, Desc: "see and change every setting, and save your picks to init.lua",
		GoFn: func(h *Host, args string) error { return h.configCommand(args) },
		GoComplete: subcommandCompleter([]string{"set", "toggle", "reset", "pick", "lua", "save", "open"}, func(h *Host, sub string) []string {
			if sub == "lua" || sub == "save" || sub == "open" {
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
		o := Overlay{ID: "config", Title: "Config", Origin: lua.OriginBuiltin, Fields: true, Items: h.configItems(),
			Reload: func() ([]any, [][]Segment) { return h.configItems(), nil }}
		if e := h.InitError(); e != "" { // what init.lua sets past the error never ran
			o.Body = "error: " + e
		}
		ui.OpenOverlay(o)
	case "toggle", "reset":
		s, err := h.setting(path)
		if err != nil {
			return err
		}
		if verb == "reset" {
			return h.SetConfig(path, s.Default)
		}
		if s.Kind != config.KindBool {
			return Usagef("%s is not on or off; /config set %s <value>", path, path)
		}
		return h.SetConfig(path, !h.Settings.Bool(path))
	case "pick":
		return h.openConfigPick(ui, path)
	case "lua":
		body, unsaved := h.configLua()
		o := Overlay{ID: "config lua", Title: "Config as Lua", Origin: lua.OriginBuiltin, Body: body}
		if unsaved {
			o.Items = []any{map[string]any{"text": "Save unsaved to init.lua", "verb": "save", "run": "config save"}}
		}
		ui.OpenOverlay(o)
	case "save":
		h.GoSafe("config save", h.saveConfig)
	case "open":
		// the editor blocks and a Go command runs on the key loop
		h.GoSafe("config open", func() {
			if err := ui.EditFile(context.Background(), EditRequest{Path: filepath.Join(h.Dirs.ConfigHome, "init.lua"), Title: "init.lua"}); err != nil {
				h.Notify(logging.Error, "Could not open init.lua", err.Error())
			}
		})
	default:
		return Usagef("usage: /config [set <key> <value> | toggle <key> | reset <key> | pick <key> | lua | save | open]")
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
	case "agent.effort":
		if s == "default" {
			val = ""
		}
	}
	return h.setWatched(path, val)
}

// configItems lists every setting by section under two action rows. Enter
// opens a row's detail, whose actions change the value; space toggles a
// bool and passes the other rows by.
func (h *Host) configItems() []any {
	items := []any{
		map[string]any{"text": "Show as Lua", "verb": "show", "run": "config lua"},
		map[string]any{"text": "Open init.lua", "verb": "open", "run": "config open"},
	}
	for _, s := range h.Settings.Schema() {
		v, _ := h.Settings.Get(s.Path)
		source, value := h.cfgSource(s.Path), showSetting(v)+" · "
		if s.Kind == config.KindBool {
			value = "" // the switch in the key column draws it
		}
		section, _, _ := strings.Cut(s.Path, ".")
		// The source leads every row's description, since a bool's key column
		// holds its switch.
		item := map[string]any{
			"text": s.Path, "desc": source + " · " + value + s.Desc, "section": sourceSection(section),
			"detail": fmt.Sprintf("kind: %s\ndefault: %s\nvalue: %s\nsource: %s\n\n%s", s.Kind, showSetting(s.Default), showSetting(v), source, s.Desc),
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
		if !reflect.DeepEqual(v, s.Default) {
			actions = append(actions, map[string]any{"text": "Reset to default", "hotkey": "r", "run": "config reset " + s.Path})
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

// configLua is every setting off its default or unsaved as the lines
// init.lua sets it with, and whether any is unsaved.
func (h *Host) configLua() (string, bool) {
	lines, unsaved := []string{cfgBlockStart}, false
	for _, s := range h.Settings.Schema() {
		v, _ := h.Settings.Get(s.Path)
		source := h.cfgSource(s.Path)
		if source != "unsaved" && reflect.DeepEqual(v, s.Default) {
			continue
		}
		line := setLine(s.Path, luaLiteral(v))
		if source == "unsaved" {
			line, unsaved = line+" -- unsaved", true
		}
		lines = append(lines, line)
	}
	return strings.Join(append(lines, cfgBlockEnd), "\n"), unsaved
}

func setLine(path, lit string) string { return fmt.Sprintf("zemi.config.set(%q, %s)", path, lit) }

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

// cfgLine is one setting to save: its path and its value as a Lua literal.
type cfgLine struct{ path, lit string }

// saveConfig writes the unsaved settings into init.lua and toasts what it
// saved and what it left alone.
func (h *Host) saveConfig() {
	var sets []cfgLine
	for _, s := range h.Settings.Schema() {
		if h.cfgSource(s.Path) == "unsaved" {
			v, _ := h.Settings.Get(s.Path)
			sets = append(sets, cfgLine{s.Path, luaLiteral(v)})
		}
	}
	if len(sets) == 0 {
		h.Notify(logging.Info, "Nothing to save", "init.lua sets every value in force")
		return
	}
	path := filepath.Join(h.Dirs.ConfigHome, "init.lua")
	read, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		h.Notify(logging.Error, "Could not save to init.lua", err.Error())
		return
	}
	saved, skipped, err := saveInit(path, read, sets)
	if err != nil {
		h.Notify(logging.Error, "Could not save to init.lua", err.Error())
		return
	}
	var body []string
	if len(saved) > 0 {
		body = append(body, strings.Join(saved, ", "))
	}
	for _, p := range skipped {
		body = append(body, p+" is set by code or more than once in init.lua; left alone")
	}
	if len(saved) == 0 {
		h.Notify(logging.Warn, "Nothing saved to init.lua", strings.Join(body, "\n"))
		return
	}
	h.Notify(logging.Info, fmt.Sprintf("Saved %d %s to init.lua", len(saved), plural(len(saved), "setting", "settings")), strings.Join(body, "\n"))
}

// saveInit writes sets into the init.lua at path, read before, and refuses
// when the file changed since: writing over it would drop that change. The
// write is a rename, so an editor or a crash never sees half a file.
func saveInit(path string, read []byte, sets []cfgLine) (saved, skipped []string, err error) {
	out, saved, skipped := rewriteInit(string(read), sets)
	if len(saved) == 0 {
		return nil, skipped, nil
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real // a linked init.lua stays a link
	}
	now, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	if !bytes.Equal(now, read) {
		return nil, nil, errors.New("init.lua changed while saving; nothing was written")
	}
	mode := fs.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".init.lua.*")
	if err != nil {
		return nil, nil, err
	}
	defer os.Remove(tmp.Name()) // names nothing once renamed
	_, err = tmp.WriteString(out)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return nil, nil, err
	}
	return saved, skipped, nil
}

// rewriteInit sets each key in src: the one top-level zemi.config.set line
// naming it with a literal gets the new literal, a key no line names joins
// the managed block at the end, and a key set by code, indented or twice is
// left alone and returned in skipped. No other line changes.
// NOTE: lines are matched one by one, so a set line inside a block comment
// or a long string reads as code, a new block lands after a final top-level
// return (which Lua refuses), and a start marker with no end gets a second
// block; a Lua parser would lift all three.
func rewriteInit(src string, sets []cfgLine) (out string, saved, skipped []string) {
	lines := strings.Split(src, "\n")
	var add []string
	for _, s := range sets {
		q := `zemi\.config\.set\(\s*["']` + regexp.QuoteMeta(s.path) + `["']`
		names := regexp.MustCompile(q)
		literal := regexp.MustCompile(`^` + q + `\s*,\s*("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|-?[0-9][0-9.]*|true|false)\s*\)\s*(?:--.*)?$`)
		var at []int
		var span []int
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "--") || !names.MatchString(l) {
				continue
			}
			at = append(at, i)
			if m := literal.FindStringSubmatchIndex(l); m != nil {
				span = m[2:4]
			}
		}
		switch {
		case len(at) == 0:
			add = append(add, setLine(s.path, s.lit))
		case len(at) == 1 && span != nil:
			l := lines[at[0]]
			lines[at[0]] = l[:span[0]] + s.lit + l[span[1]:]
		default:
			skipped = append(skipped, s.path)
			continue
		}
		saved = append(saved, s.path)
	}
	if len(add) == 0 {
		return strings.Join(lines, "\n"), saved, skipped
	}
	if start := slices.Index(lines, cfgBlockStart); start >= 0 {
		if end := slices.Index(lines[start:], cfgBlockEnd); end >= 0 {
			return strings.Join(slices.Insert(lines, start+end, add...), "\n"), saved, skipped
		}
	}
	out = strings.Join(lines, "\n")
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if out != "" {
		out += "\n"
	}
	return out + strings.Join(slices.Concat([]string{cfgBlockStart}, add, []string{cfgBlockEnd}), "\n") + "\n", saved, skipped
}
