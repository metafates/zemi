package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
)

func (h *Host) uiNamespace(L *glua.LState, sb *lua.Sandbox) *glua.LTable {
	tb := L.NewTable()

	widget := L.NewTable()
	widget.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		tb, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.ui.widget.add: expected a spec table")
		}
		w := &Widget{
			ID:     rawString(L, tb, "id"),
			Region: rawString(L, tb, "region"),
			Wrap:   rawBool(L, tb, "wrap"),
			Origin: h.RT.CurrentOrigin(L),
			Path:   h.RT.CurrentPath(L),
			Gen:    h.currentGen(),
		}
		if n, ok := opt[glua.LNumber](L, tb, "priority"); ok {
			w.Priority = int(n)
		}
		// the exit region prints to the shell, outside zemi's chrome, beside the
		// resume command a user copies
		if w.Region == RegionExit && w.Origin.Rank() < lua.OriginBuiltin.Rank() {
			L.RaiseError("zemi.ui.widget.add: only user config draws the exit region")
		}
		if fn, ok := opt[*glua.LFunction](L, tb, "render"); ok {
			w.Render = fn
		}
		if fn, ok := opt[*glua.LFunction](L, tb, "on_click"); ok {
			w.OnClick = fn
		}
		if _, err := h.Widgets.Add(w); err != nil {
			L.RaiseError("zemi.ui.widget.add: %v", err)
		}
		h.refresh()
		return 0
	}))
	widget.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		ok, err := h.Widgets.Remove(L.CheckString(1), h.RT.CurrentOrigin(L), h.RT.CurrentPath(L))
		if err != nil {
			L.RaiseError("zemi.ui.widget.del: %v", err)
		}
		h.refresh()
		L.Push(glua.LBool(ok))
		return 1
	}))
	widget.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		rows := []any{}
		for _, w := range h.Widgets.List() {
			rows = append(rows, map[string]any{"id": w.ID, "region": w.Region, "priority": w.Priority, "origin": string(w.Origin)})
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))
	tb.RawSetString("widget", widget)
	tb.RawSetString("layout", h.layoutNamespace(L))
	focus := h.focusNamespace(L)
	tb.RawSetString("focus", focus)

	theme := L.NewTable()
	theme.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		m := tableArg(L, 2, "zemi.ui.theme.add")
		t := &Theme{
			Name:    name,
			Palette: map[string]string{},
			HL:      map[string]HLSpec{},
			Origin:  h.RT.CurrentOrigin(L),
			Path:    h.RT.CurrentPath(L),
			Gen:     h.currentGen(),
		}
		if pal, ok := m["palette"].(map[string]any); ok {
			for k, v := range pal {
				s, _ := v.(string)
				if !isHexColor(s) {
					L.RaiseError("zemi.ui.theme.add: palette %s = %q is not #rrggbb; a palette holds colors, not names", k, s)
				}
				t.Palette[k] = s
			}
		}
		if hl, ok := m["hl"].(map[string]any); ok {
			for k, v := range hl {
				if spec, ok := v.(map[string]any); ok {
					t.HL[k] = hlSpec(L, "zemi.ui.theme.add", spec, t.Palette)
				}
			}
		}
		if _, err := h.Themes.Add(t); err != nil {
			L.RaiseError("zemi.ui.theme.add: %v", err)
		}
		return 0
	}))
	theme.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		h.refuseBelowUser(L, "ui.theme.set") // it also writes the user-only ui.theme setting
		name := L.CheckString(1)
		if h.Themes.Get(name) != nil && h.cfgReplayed("ui.theme", name) {
			return 0
		}
		if err := h.SetTheme(name); err != nil {
			L.RaiseError("zemi.ui.theme.set: %v", err)
		}
		return 0
	}))
	theme.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		L.Push(glua.LString(h.ActiveThemeName()))
		return 1
	}))
	theme.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		rows := []any{}
		for _, t := range h.Themes.List() {
			rows = append(rows, map[string]any{"name": t.Name, "origin": string(t.Origin)})
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))
	tb.RawSetString("theme", theme)

	overlay := L.NewTable()
	overlay.RawSetString("open", L.NewFunction(func(L *glua.LState) int {
		tb, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.ui.overlay.open: expected a spec table")
		}
		o := Overlay{
			ID:     rawString(L, tb, "id"),
			Title:  rawString(L, tb, "title"),
			BodyMD: rawBool(L, tb, "markdown"),
			Empty:  rawString(L, tb, "empty"),
			Origin: h.RT.CurrentOrigin(L),
			Path:   h.RT.CurrentPath(L),
			Gen:    h.currentGen(),
			Anchor: OverlayAnchor(rawString(L, tb, "anchor")),
		}
		if o.Anchor != "" && !slices.Contains(overlayAnchors, o.Anchor) {
			L.RaiseError("zemi.ui.overlay.open: anchor must be one of %s or nil, got %q", strings.Join(strs(overlayAnchors), ", "), o.Anchor)
		}
		switch body := tb.RawGetString("body").(type) {
		case glua.LString:
			o.Body = string(body)
		case *glua.LTable:
			v, err := lua.ToGo(body)
			if err == nil {
				o.BodySegs, err = convertSegments(v, "")
			}
			if err != nil {
				L.RaiseError("zemi.ui.overlay.open: body: %v", err)
			}
			for _, row := range o.BodySegs {
				for _, seg := range row {
					o.Body += seg.Text
				}
				o.Body += "\n"
			}
		default:
			if body != glua.LNil {
				L.RaiseError("body must be a string or a table of lines, not a %s", body.Type())
			}
		}
		if n, ok := opt[glua.LNumber](L, tb, "selected"); ok {
			o.Selected = int(n) - 1
		}
		if v := tb.RawGetString("items"); v != glua.LNil {
			items, err := lua.ToGo(v)
			if err == nil {
				o.Items, err = overlayItems(items)
			}
			if err != nil {
				L.RaiseError("zemi.ui.overlay.open: %v", err)
			}
		}
		render, _ := opt[*glua.LFunction](L, tb, "render_item")
		if render != nil {
			o.ItemText = h.renderItems(L, o.ID, render, o.Items)
		}
		if fn, ok := opt[*glua.LFunction](L, tb, "reload"); ok {
			o.Reload = h.luaReload(o, fn, render)
		}
		if fn, ok := opt[*glua.LFunction](L, tb, "on_select"); ok {
			o.OnSelect = fn
		}
		if fn, ok := opt[*glua.LFunction](L, tb, "on_action"); ok {
			o.OnAction = fn
		}
		if fn, ok := opt[*glua.LFunction](L, tb, "on_close"); ok {
			o.OnClose = fn
		}
		if keys, ok := opt[*glua.LTable](L, tb, "keys"); ok {
			keys.ForEach(func(k, v glua.LValue) {
				spec, _ := v.(*glua.LTable)
				if spec == nil {
					spec = L.NewTable()
				}
				fn, _ := opt[*glua.LFunction](L, spec, "fn")
				// one character, so no overlay can take esc or ctrl+c
				if key := k.String(); utf8.RuneCountInString(key) != 1 || fn == nil {
					L.RaiseError("zemi.ui.overlay.open: keys[%q] must be one character with { desc, fn }", key)
				}
				if strings.Contains(listOwnKeys, k.String()) {
					L.RaiseError("zemi.ui.overlay.open: keys[%q] is the list's own key", k.String())
				}
				o.Keys = append(o.Keys, OverlayKey{Key: k.String(), Desc: rawString(L, spec, "desc"), Fn: fn})
			})
			slices.SortFunc(o.Keys, func(a, b OverlayKey) int { return cmp.Compare(a.Key, b.Key) })
		}
		if o.ID == "" {
			L.RaiseError("zemi.ui.overlay.open: id is required")
		}
		if ui := h.hooks(); ui != nil {
			ui.OpenOverlay(o)
		}
		return 0
	}))
	overlay.RawSetString("close", L.NewFunction(func(L *glua.LState) int {
		id := L.CheckString(1)
		if ui := h.hooks(); ui != nil {
			ui.CloseOverlay(id, h.RT.CurrentOrigin(L))
		}
		return 0
	}))
	tb.RawSetString("overlay", overlay)

	// select(items, opts, on_choice): a picker in the shape of vim.ui.select
	// over the overlay stack. on_choice(item) runs on enter, on_choice(nil)
	// on cancel; opts.prompt titles it, opts.format_item renders rows.
	tb.RawSetString("select", L.NewFunction(func(L *glua.LState) int {
		itemsV := L.CheckTable(1)
		opts := L.OptTable(2, L.NewTable())
		cb := L.CheckFunction(3)
		items, err := lua.ToGo(itemsV)
		if err != nil {
			L.RaiseError("zemi.ui.select: %v", err)
		}
		list, ok := items.([]any)
		if m, empty := items.(map[string]any); empty && len(m) == 0 {
			ok = true // Lua's {} marshals as an empty map: no items
		}
		if !ok {
			L.RaiseError("zemi.ui.select: items must be a list; the order is select(items, opts, on_choice)")
		}
		o := Overlay{
			ID:       fmt.Sprintf("select:%d", h.selectSeq.Add(1)),
			Title:    rawString(L, opts, "prompt"),
			Origin:   h.RT.CurrentOrigin(L),
			Path:     h.RT.CurrentPath(L),
			Gen:      h.currentGen(),
			Items:    list,
			OnSelect: cb,
			OnClose:  cb, // cb() gets nil, the vim.ui cancel shape
		}
		if fn, ok := opt[*glua.LFunction](L, opts, "format_item"); ok {
			o.ItemText = h.renderItems(L, o.ID, fn, list)
		}
		if ui := h.hooks(); ui != nil {
			ui.OpenOverlay(o)
		}
		return 0
	}))

	// input(opts, on_confirm): a one-line prompt in the shape of
	// vim.ui.input. on_confirm(text) runs on enter, on_confirm(nil) on esc;
	// opts.prompt titles it, opts.default prefills it.
	tb.RawSetString("input", L.NewFunction(func(L *glua.LState) int {
		opts := L.OptTable(1, L.NewTable())
		cb := L.CheckFunction(2)
		o := Overlay{
			ID:       fmt.Sprintf("input:%d", h.selectSeq.Add(1)),
			Title:    rawString(L, opts, "prompt"),
			Origin:   h.RT.CurrentOrigin(L),
			Path:     h.RT.CurrentPath(L),
			Gen:      h.currentGen(),
			Input:    true,
			Default:  rawString(L, opts, "default"),
			OnSelect: cb,
			OnClose:  cb,
		}
		if ui := h.hooks(); ui != nil {
			ui.OpenOverlay(o)
		}
		return 0
	}))

	// edit(path, opts, on_done): the user's editor on a project file, then
	// on_done() once it closes, or on_done(err). Never blocks the
	// interpreter. The gate is a read: the writing is the user's own.
	tb.RawSetString("edit", L.NewFunction(func(L *glua.LState) int {
		path := L.CheckString(1)
		opts := L.OptTable(2, L.NewTable())
		cb := L.CheckFunction(3)
		roots := h.luaJails(L, true)
		if h.ProjectTrusted() {
			// the editor is the user's own hands, and zemi.project.root() is
			// where a job's paths are relative to
			roots = append(roots, h.Dirs.RepoRoot())
		}
		// the editor opens the path by name and follows symlinks anyway
		p, err := h.resolveJailed(path, roots, false)
		if err != nil {
			L.RaiseError("zemi.ui.edit: %v", err)
		}
		resolved := p.String()
		// gated by the written name like zemi.fs, so a secret-named link asks
		h.luaGate(L, "lua.fs.read", h.projectAbs(path), nil)
		line := 0
		if n, ok := opt[glua.LNumber](L, opts, "line"); ok {
			line = int(n)
		}
		title := rawString(L, opts, "title")
		if title == "" {
			title = filepath.Base(resolved)
		}
		at := h.callerOf(L)
		ui := h.hooks()
		h.GoSafe("ui.edit", func() {
			if ui == nil {
				h.callLuaAsync("zemi.ui.edit on_done", at, cb, "no UI in this run")
				return
			}
			h.lspDirty()
			if err := ui.EditFile(context.Background(), EditRequest{Path: resolved, Line: line, Title: title}); err != nil {
				h.callLuaAsync("zemi.ui.edit on_done", at, cb, err.Error())
				return
			}
			h.callLuaAsync("zemi.ui.edit on_done", at, cb)
		})
		return 0
	}))

	// width(text) is the cells the text draws in: a wide glyph (CJK, emoji)
	// is two, so Lua clips and pads the way the renderer measures
	tb.RawSetString("width", L.NewFunction(func(L *glua.LState) int {
		L.Push(glua.LNumber(ansi.StringWidth(L.CheckString(1))))
		return 1
	}))

	tb.RawSetString("markdown", L.NewFunction(func(L *glua.LState) int {
		L.Push(linesTable(L, h.Markdown(L.CheckString(1), L.CheckInt(2))))
		return 1
	}))
	tb.RawSetString("highlight", L.NewFunction(func(L *glua.LState) int {
		L.Push(linesTable(L, h.Highlight(L.CheckString(1), L.CheckString(2), L.OptInt(3, 0))))
		return 1
	}))
	tb.RawSetString("wrap", L.NewFunction(func(L *glua.LState) int {
		L.Push(linesTable(L, WrapText(L.CheckString(1), L.CheckInt(2))))
		return 1
	}))
	tb.RawSetString("diff", L.NewFunction(func(L *glua.LState) int {
		L.Push(linesTable(L, DiffRows(L.CheckString(1), L.CheckInt(2))))
		return 1
	}))

	hl := L.NewTable()
	// hl.set(group, spec) changes one highlight group on top of the active
	// theme (and any theme set later): { fg, bg, sp, bold, italic, underline,
	// reverse, brighten, replace }, colors as hex or palette names, or
	// { link = "Group" }. theme.add builds a whole theme.
	hl.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		// an override sits above every theme, the user's own included
		h.refuseBelowUser(L, "ui.hl.set")
		group := L.CheckString(1)
		// an override outlives the theme active when it is set, so any
		// registered theme's palette may name the color
		h.Themes.SetHL(group, hlSpec(L, "zemi.ui.hl.set", tableArg(L, 2, "zemi.ui.hl.set"), h.Themes.Palettes()))
		h.refresh()
		return 0
	}))

	// hl.get(group) is how the group draws under the active theme, links
	// followed and palette names resolved to hex (an ANSI color name is the
	// terminal's, so it stays a name), or nil when nothing sets it.
	hl.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		theme := h.Themes.Get(h.ActiveThemeName())
		spec, ok := h.Themes.Group(theme, L.CheckString(1))
		if !ok {
			L.Push(glua.LNil)
			return 1
		}
		color := func(ref string) any {
			if theme != nil {
				if v := theme.Palette[ref]; v != "" {
					ref = v // an ANSI color name has none and stays as written
				}
			}
			if ref == "" {
				return nil
			}
			return ref
		}
		flag := func(b *bool) any {
			if b == nil {
				return nil
			}
			return *b
		}
		out := map[string]any{
			"fg": color(spec.Fg), "bg": color(spec.Bg), "sp": color(spec.Sp),
			"bold": flag(spec.Bold), "italic": flag(spec.Italic), "reverse": flag(spec.Reverse),
		}
		switch spec.Underline {
		case "":
		case UnderlineNone:
			out["underline"] = false
		case UnderlineSingle:
			out["underline"] = true
		default:
			out["underline"] = spec.Underline
		}
		if spec.Brighten != 0 {
			out["brighten"] = spec.Brighten
		}
		if spec.Replace {
			out["replace"] = true
		}
		L.Push(lua.FromGo(L, out))
		return 1
	}))
	tb.RawSetString("hl", hl)

	reasoning := L.NewTable()
	reasoning.RawSetString("toggle", L.NewFunction(func(L *glua.LState) int {
		h.ToggleReasoning()
		return 0
	}))
	tb.RawSetString("reasoning", reasoning)

	// screenshot() -> the drawn screen as plain text, or nil, err. zemi
	// reads its own UI with it instead of asking what the user sees.
	tb.RawSetString("screenshot", L.NewFunction(func(L *glua.LState) int {
		ui := h.hooks()
		if ui == nil {
			return luaResult(L, errors.New("no TUI in this run"))
		}
		shot := ui.Screenshot()
		if shot == "" {
			return luaResult(L, errors.New("nothing drawn yet"))
		}
		L.Push(glua.LString(shot))
		return 1
	}))

	// redraw() renders every widget again on the next frame: for state a
	// repeating timer or code outside a callback changed.
	tb.RawSetString("redraw", L.NewFunction(func(L *glua.LState) int {
		h.refresh()
		return 0
	}))

	// scroll(action, id?) is the zemi.ui.scroll.* host actions for a click.
	tb.RawSetString("scroll", L.NewFunction(func(L *glua.LState) int {
		kind := ScrollAction(L.CheckString(1))
		if !slices.Contains(scrollActions, kind) {
			L.ArgError(1, strings.Join(strs(scrollActions), ", "))
		}
		h.scroll(kind, L.OptString(2, ""))
		return 0
	}))

	// title(text?) sets the terminal tab's title; nil gives it back to the
	// session. A reload that does not set it again gives it back too.
	tb.RawSetString("title", L.NewFunction(func(L *glua.LState) int {
		h.refuseBelowUser(L, "ui.title")
		text := sanitize.Line(L.OptString(1, ""))
		if text == "" {
			h.title.Store(nil)
		} else {
			h.title.Store(&titleSet{text: text, gen: h.currentGen()})
		}
		h.refresh()
		return 0
	}))

	tb.RawSetString("help", L.NewFunction(func(L *glua.LState) int {
		_ = h.OpenHelp() // headless there is nothing to open it on, like every overlay
		return 0
	}))

	sb.FreezeTable(widget, "namespace zemi.ui.widget")
	sb.FreezeTable(theme, "namespace zemi.ui.theme")
	sb.FreezeTable(overlay, "namespace zemi.ui.overlay")
	sb.FreezeTable(focus, "namespace zemi.ui.focus")
	sb.FreezeTable(hl, "namespace zemi.ui.hl")

	return tb
}

// tableArg reads argument n as a table; anything else raises (DESIGN §6.0),
// since a missing spec would otherwise read as an empty one.
func tableArg(L *glua.LState, n int, where string) map[string]any {
	m, ok := luaToGoChecked(L, n, where).(map[string]any)
	if !ok {
		L.RaiseError("%s: argument %d must be a table", where, n)
	}
	return m
}

// isHexColor reports whether ref is #rgb or #rrggbb, the only color
// literal a theme understands.
func isHexColor(ref string) bool {
	s, ok := strings.CutPrefix(ref, "#")
	if !ok || (len(s) != 3 && len(s) != 6) {
		return false
	}
	_, err := strconv.ParseUint(s, 16, 32)
	return err == nil
}

// ANSIColors are the terminal's own colors: a group names one instead of a
// hex value to let the user's terminal palette decide it.
var ANSIColors = map[string]ansi.BasicColor{
	"black": ansi.Black, "red": ansi.Red, "green": ansi.Green, "yellow": ansi.Yellow,
	"blue": ansi.Blue, "magenta": ansi.Magenta, "cyan": ansi.Cyan, "white": ansi.White,
}

// checkColor refuses a color that can never resolve: a name outside the
// palette would draw as nothing, and a CSS color name is the first thing
// anyone writes (DESIGN §6.0).
func checkColor(L *glua.LState, where, field, ref string, palette map[string]string) {
	_, isANSI := ANSIColors[ref]
	switch {
	case ref == "" || ref == "NONE" || isANSI || palette[ref] != "":
	case strings.HasPrefix(ref, "#") && isHexColor(ref):
	case strings.HasPrefix(ref, "#"):
		L.RaiseError("%s: %s %q is not a hex color; write #rrggbb", where, field, ref)
	default:
		L.RaiseError("%s: %s %q is not #rrggbb, an ANSI color name, or a palette key (%s)",
			where, field, ref, strings.Join(slices.Sorted(maps.Keys(palette)), ", "))
	}
}

// hlSpec reads a group table; misuse raises (DESIGN §6.0). palette is what
// its colors may name.
func hlSpec(L *glua.LState, where string, m map[string]any, palette map[string]string) HLSpec {
	flag := func(key string) *bool {
		if b, ok := m[key].(bool); ok {
			return &b
		}
		return nil
	}
	for _, f := range []string{"fg", "bg", "sp"} {
		checkColor(L, where, f, strField(m, f), palette)
	}
	spec := HLSpec{
		Link: strField(m, "link"),
		Fg:   strField(m, "fg"), Bg: strField(m, "bg"), Sp: strField(m, "sp"),
		Bold: flag("bold"), Italic: flag("italic"), Reverse: flag("reverse"),
		Replace: boolField(m, "replace"),
	}
	switch u := m["underline"].(type) {
	case nil:
	case bool:
		spec.Underline = map[bool]Underline{true: UnderlineSingle, false: UnderlineNone}[u]
	case string:
		if !slices.Contains(underlines, Underline(u)) {
			L.RaiseError("%s: underline %q is not one of %s", where, u, strings.Join(strs(underlines), ", "))
		}
		spec.Underline = Underline(u)
	default:
		L.RaiseError("%s: underline must be a boolean or a string", where)
	}
	if b, ok := m["brighten"].(float64); ok {
		spec.Brighten = max(-1, min(b, 1))
	}
	return spec
}

// OverlayItem is an overlay row's fields, parsed from its item: a table of
// them, else a value whose text is the row. Callbacks get the raw item.
type OverlayItem struct {
	Text, Desc, Key, Section, Detail, Verb, Hotkey, Fill, Copy, Run, ID, Preview string
	Switch, On, Locked                                                           bool    // Switch: on is set, so the row draws the switch
	Danger                                                                       bool    // cannot be undone: drawn in Danger, and its first enter or key only arms it
	Tail                                                                         bool    // a text too wide is cut from the left: a path's name is at its end
	Actions                                                                      []any   // a detail view's items; nil when it has none
	Fields                                                                       []Field // the detail view's facts, set above Detail
}

// Field is a name and value a detail view sets as a table above its text.
type Field struct{ Name, Value string }

// fieldList is an item's "fields" from name, value pairs, in the shape a Lua
// list marshals to, so Go items stay plain Lua data.
func fieldList(kv ...string) []any {
	if len(kv)%2 != 0 {
		panic("fieldList: a name without a value")
	}
	out := make([]any, 0, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		out = append(out, []any{kv[i], kv[i+1]})
	}
	return out
}

// ParseOverlayItem reads item's fields, leaving out one of the wrong type,
// which err names. A list item's id is its own data; a detail action's is
// checked.
func ParseOverlayItem(item any) (OverlayItem, error) { return parseItem(item, false) }

// listOwnKeys are the one-character keys every list keeps for itself, so
// no overlay key or hotkey may take them.
const listOwnKeys = "jk/yY"

func parseItem(item any, action bool) (OverlayItem, error) {
	m, ok := item.(map[string]any)
	if !ok {
		s, ok := item.(string)
		if !ok {
			s = fmt.Sprint(item)
		}
		return OverlayItem{Text: s}, nil
	}
	var p OverlayItem
	var err error
	bad := func(key, want string, v any) {
		if err == nil {
			err = fmt.Errorf("%s must be %s, got %s", key, want, luaShape(v))
		}
	}
	for _, f := range []struct {
		key string
		dst *string
	}{{"text", &p.Text}, {"desc", &p.Desc}, {"key", &p.Key}, {"section", &p.Section}, {"detail", &p.Detail}, {"verb", &p.Verb}, {"hotkey", &p.Hotkey}, {"fill", &p.Fill}, {"copy", &p.Copy}, {"run", &p.Run}, {"id", &p.ID}, {"preview", &p.Preview}} {
		switch v := m[f.key].(type) {
		case nil:
		case string:
			*f.dst = v
		default:
			if action || f.key != "id" {
				bad(f.key, "a string", v)
			}
		}
	}
	if _, ok := m["text"].(string); !ok {
		p.Text = fmt.Sprint(item)
	}
	for _, f := range []struct {
		key string
		dst *bool
	}{{"on", &p.On}, {"locked", &p.Locked}, {"danger", &p.Danger}, {"tail", &p.Tail}} {
		switch v := m[f.key].(type) {
		case nil:
		case bool:
			*f.dst = v
		default:
			bad(f.key, "a boolean", v)
		}
	}
	_, p.Switch = m["on"].(bool)
	switch v := m["fields"].(type) {
	case nil:
	case []any:
		for _, f := range v {
			pair, _ := f.([]any)
			name, value, ok := "", "", len(pair) == 2
			if ok {
				name, ok = pair[0].(string)
			}
			if ok {
				value, ok = pair[1].(string)
			}
			if !ok {
				bad("fields", "a list of { name, value } pairs", v)
				break
			}
			p.Fields = append(p.Fields, Field{name, value})
		}
	case map[string]any:
		if len(v) > 0 {
			bad("fields", "a list of { name, value } pairs", v)
		}
	default:
		bad("fields", "a list of { name, value } pairs", v)
	}
	switch v := m["actions"].(type) {
	case nil:
	case []any:
		p.Actions = v
	case map[string]any:
		if len(v) > 0 {
			bad("actions", "a list", v)
			break
		}
		p.Actions = []any{} // Lua's {} marshals as an empty map
	default:
		bad("actions", "a list", v)
	}
	if h := p.Hotkey; h != "" && (utf8.RuneCountInString(h) != 1 || strings.Contains(listOwnKeys, h)) && err == nil {
		err = fmt.Errorf("hotkey must be one character other than j, k, /, y and Y, got %q", h)
	}
	for i, a := range p.Actions {
		if _, e := parseItem(a, true); e != nil && err == nil {
			err = fmt.Errorf("actions[%d].%w", i+1, e)
		}
	}
	return p, err
}

// overlayItems is a Lua items list with every item's fields checked.
func overlayItems(v any) ([]any, error) {
	if m, ok := v.(map[string]any); ok && len(m) == 0 {
		return nil, nil // Lua's {} marshals as an empty map
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("items must be a list, got %s", luaShape(v))
	}
	for i, item := range list {
		if _, err := ParseOverlayItem(item); err != nil {
			return nil, fmt.Errorf("items[%d].%w", i+1, err)
		}
	}
	return list, nil
}

// renderItems runs an overlay's render_item here on the interpreter, so the
// UI goroutine never waits on the queue to draw the rows. render_item
// returns a string or one line of segments; a failing item falls back to its
// plain value.
func (h *Host) renderItems(L *glua.LState, id string, fn *glua.LFunction, items []any) [][]Segment {
	out := make([][]Segment, len(items))
	for i, item := range items {
		base := L.GetTop()
		L.Push(fn)
		L.Push(lua.FromGo(L, item))
		err := L.PCall(1, 1, nil)
		if err == nil {
			switch v := L.Get(-1).(type) {
			case *glua.LTable:
				var line any
				if line, err = lua.ToGo(v); err == nil {
					var rows [][]Segment
					if rows, err = convertSegments([]any{line}, ""); err == nil {
						out[i] = rows[0]
					}
				}
			case *glua.LNilType:
			default:
				out[i] = []Segment{{Text: v.String()}}
			}
		}
		if err != nil {
			h.Log.Debug("ui", "overlay render_item failed", map[string]any{"id": id, "error": err.Error()})
			out[i] = []Segment{{Text: fmt.Sprint(item)}}
		}
		L.SetTop(base)
	}
	return out
}

// luaReload is a Lua overlay's reload: fn(), a task, returns the new items,
// which render_item renders again, under the overlay's origin. A failure or a
// stale generation keeps the items the overlay has.
func (h *Host) luaReload(o Overlay, fn, render *glua.LFunction) func() ([]any, [][]Segment) {
	return func() ([]any, [][]Segment) {
		items, text := o.Items, o.ItemText
		t := h.luaTask(lua.ClassUI, luaCaller{gen: o.Gen, origin: o.Origin, path: o.Path}, fn)
		t.Done = func(L *glua.LState, rets []glua.LValue) ([]any, error) {
			v, err := lua.ToGo(rets[0])
			var list []any
			if err == nil {
				list, err = overlayItems(v)
			}
			if err != nil {
				return nil, err
			}
			items, text = list, nil
			if render != nil {
				text = h.renderItems(L, o.ID, render, list)
			}
			return nil, nil
		}
		_, err := h.RT.RunTask(context.Background(), t)
		if err != nil && !lua.Dropped(err) {
			h.RecordError("overlay "+o.ID+" reload", err.Error())
		}
		return items, text
	}
}

// EmitAsync delivers a host event without blocking the caller: it is safe
// from the interpreter goroutine (a blocking Emit there would deadlock the
// queue). Stale-generation deliveries are dropped. The event is recorded
// either way; with no handler that is all that happens.
func (h *Host) EmitAsync(name string, payload map[string]any) {
	h.recordEvent(name, "host", payload)
	if len(h.Events.Handlers(name)) == 0 {
		return
	}
	gen := h.currentGen()
	h.GoSafe("event "+name, func() {
		_, err := h.RT.Do(context.Background(), lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
			if h.currentGen() != gen {
				return nil, nil
			}
			h.emitSync(L, h.Events.Handlers(name), name, payload, "host")
			return nil, nil
		})
		h.refresh()
		if err != nil {
			h.Log.Debug("events", "async emit dropped", map[string]any{"event": name, "error": err.Error()})
		}
	})
}

// SetTheme makes a registered theme the active one.
func (h *Host) SetTheme(name string) error {
	if h.Themes.Get(name) == nil {
		return Usagef("unknown theme %q", name)
	}
	if err := h.setWatched("ui.theme", name); err != nil {
		return err
	}
	h.EmitAsync("ui.theme", map[string]any{"theme": name})
	return nil
}

// ActiveThemeName returns the configured theme, or its "-light" variant
// when the terminal is light and the theme registered one.
func (h *Host) ActiveThemeName() string {
	name := cmp.Or(h.Settings.Str("ui.theme"), "whorl")
	if h.lightTerm.Load() && h.Themes.Get(name+"-light") != nil {
		return name + "-light"
	}
	return name
}

// SetLightTerminal records whether the terminal's background is light.
func (h *Host) SetLightTerminal(light bool) { h.lightTerm.Store(light) }

// linesTable is segment rows as zemi.Line tables.
func linesTable(L *glua.LState, lines [][]Segment) *glua.LTable {
	// NewTable reserves 32 array and 32 hash slots, which a long chat
	// pays for once per segment
	out := L.CreateTable(len(lines), 0)
	for _, line := range lines {
		row := L.CreateTable(len(line), 0)
		for _, s := range line {
			seg := L.CreateTable(0, 4)
			seg.RawSetString("text", glua.LString(s.Text))
			if s.HL != "" {
				seg.RawSetString("hl", glua.LString(s.HL))
			}
			if s.NoCopy {
				seg.RawSetString("copy", glua.LFalse)
			}
			if s.Soft {
				seg.RawSetString("soft_wrap", glua.LTrue)
			}
			if s.Sticky {
				seg.RawSetString("sticky", glua.LTrue)
			}
			if s.Click != "" {
				seg.RawSetString("click", glua.LString(s.Click))
			}
			row.Append(seg)
		}
		out.Append(row)
	}
	return out
}
