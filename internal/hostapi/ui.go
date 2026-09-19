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

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
	glua "github.com/yuin/gopher-lua"
)

func (h *Host) uiNamespace(L *glua.LState, sb *lua.Sandbox) *glua.LTable {
	tb := L.NewTable()

	widget := L.NewTable()
	widget.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.ui.widget.add")
		tb, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.ui.widget.add: expected a spec table")
		}
		w := &Widget{
			ID:     rawString(tb, "id"),
			Region: rawString(tb, "region"),
			Wrap:   rawBool(tb, "wrap"),
			Origin: h.RT.CurrentOrigin(L),
			Path:   h.RT.CurrentPath(L),
			Gen:    h.currentGen(),
		}
		if n, ok := tb.RawGetString("priority").(glua.LNumber); ok {
			w.Priority = int(n)
		}
		if fn, ok := tb.RawGetString("render").(*glua.LFunction); ok {
			w.Render = fn
		}
		if fn, ok := tb.RawGetString("on_click").(*glua.LFunction); ok {
			w.OnClick = fn
		}
		if _, err := h.Widgets.Add(w); err != nil {
			L.RaiseError("zemi.ui.widget.add: %v", err)
		}
		h.refresh()
		return 0
	}))
	widget.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.ui.widget.del")
		ok, err := h.Widgets.Remove(L.CheckString(1), h.RT.CurrentOrigin(L))
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
		h.refuseDuringRender(L, "zemi.ui.theme.add")
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
		h.refuseDuringRender(L, "zemi.ui.theme.set")
		h.refuseBelowUser(L, "ui.theme.set") // it also writes the user-only ui.theme setting
		name := L.CheckString(1)
		if h.Themes.Get(name) == nil {
			L.RaiseError("zemi.ui.theme.set: unknown theme %q", name)
		}
		fired, err := h.Settings.SetQuiet("ui.theme", name)
		if err != nil {
			L.RaiseError("zemi.ui.theme.set: %v", err)
		}
		h.deliverFired(L, fired)
		h.refresh()
		h.EmitAsync("ui.theme", map[string]any{"theme": name})
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
		h.refuseDuringRender(L, "zemi.ui.overlay.open")
		tb, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.ui.overlay.open: expected a spec table")
		}
		o := Overlay{
			ID:     rawString(tb, "id"),
			Title:  rawString(tb, "title"),
			Body:   rawString(tb, "body"),
			BodyMD: tb.RawGetString("markdown") == glua.LTrue,
			Empty:  rawString(tb, "empty"),
			Origin: h.RT.CurrentOrigin(L),
			Path:   h.RT.CurrentPath(L),
			Gen:    h.currentGen(),
			Anchor: rawString(tb, "anchor"),
		}
		if o.Anchor != "" && o.Anchor != "bottom" {
			L.RaiseError("zemi.ui.overlay.open: anchor must be \"bottom\" or nil, got %q", o.Anchor)
		}
		if lines, ok := tb.RawGetString("body").(*glua.LTable); ok {
			v, err := lua.ToGo(lines)
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
		}
		if n, ok := tb.RawGetString("selected").(glua.LNumber); ok {
			o.Selected = int(n) - 1
		}
		if v := tb.RawGetString("items"); v != glua.LNil {
			items, err := lua.ToGo(v)
			if err != nil {
				L.RaiseError("zemi.ui.overlay.open: %v", err)
			}
			if list, ok := items.([]any); ok {
				o.Items = list
			}
		}
		render, _ := tb.RawGetString("render_item").(*glua.LFunction)
		if render != nil {
			o.ItemText = h.renderItems(L, o.ID, render, o.Items)
		}
		if fn, ok := tb.RawGetString("reload").(*glua.LFunction); ok {
			o.Reload = h.luaReload(o, fn, render)
		}
		if fn, ok := tb.RawGetString("on_select").(*glua.LFunction); ok {
			o.OnSelect = fn
		}
		if fn, ok := tb.RawGetString("on_action").(*glua.LFunction); ok {
			o.OnAction = fn
		}
		if fn, ok := tb.RawGetString("on_close").(*glua.LFunction); ok {
			o.OnClose = fn
		}
		if keys, ok := tb.RawGetString("keys").(*glua.LTable); ok {
			keys.ForEach(func(k, v glua.LValue) {
				spec, _ := v.(*glua.LTable)
				if spec == nil {
					spec = L.NewTable()
				}
				fn, _ := spec.RawGetString("fn").(*glua.LFunction)
				// one character, so no overlay can take esc or ctrl+c
				if key := k.String(); utf8.RuneCountInString(key) != 1 || fn == nil {
					L.RaiseError("zemi.ui.overlay.open: keys[%q] must be one character with { desc, fn }", key)
				}
				o.Keys = append(o.Keys, OverlayKey{Key: k.String(), Desc: rawString(spec, "desc"), Fn: fn})
			})
			slices.SortFunc(o.Keys, func(a, b OverlayKey) int { return cmp.Compare(a.Key, b.Key) })
		}
		if o.ID == "" {
			L.RaiseError("zemi.ui.overlay.open: id is required")
		}
		if ui := h.hooks(); ui != nil {
			ui.OpenOverlay(o)
		}
		h.EmitAsync("overlay.open", map[string]any{"id": o.ID, "title": o.Title})
		return 0
	}))
	overlay.RawSetString("close", L.NewFunction(func(L *glua.LState) int {
		id := L.CheckString(1)
		h.refuseDuringRender(L, "zemi.ui.overlay.close")
		if ui := h.hooks(); ui != nil {
			ui.CloseOverlay(id, h.RT.CurrentOrigin(L))
		}
		h.EmitAsync("overlay.close", map[string]any{"id": id})
		return 0
	}))
	tb.RawSetString("overlay", overlay)

	// select(items, opts, on_choice): a picker in the shape of vim.ui.select
	// over the overlay stack. on_choice(item) runs on enter, on_choice(nil)
	// on cancel; opts.prompt titles it, opts.format_item renders rows.
	tb.RawSetString("select", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.ui.select")
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
			Title:    rawString(opts, "prompt"),
			Origin:   h.RT.CurrentOrigin(L),
			Path:     h.RT.CurrentPath(L),
			Gen:      h.currentGen(),
			Items:    list,
			OnSelect: cb,
			OnClose:  cancelWith(L, cb),
		}
		if fn, ok := opts.RawGetString("format_item").(*glua.LFunction); ok {
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
		h.refuseDuringRender(L, "zemi.ui.input")
		opts := L.OptTable(1, L.NewTable())
		cb := L.CheckFunction(2)
		o := Overlay{
			ID:       fmt.Sprintf("input:%d", h.selectSeq.Add(1)),
			Title:    rawString(opts, "prompt"),
			Origin:   h.RT.CurrentOrigin(L),
			Path:     h.RT.CurrentPath(L),
			Gen:      h.currentGen(),
			Input:    true,
			Default:  rawString(opts, "default"),
			OnSelect: cb,
			OnClose:  cancelWith(L, cb),
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
		h.refuseDuringRender(L, "zemi.ui.edit")
		path := L.CheckString(1)
		opts := L.OptTable(2, L.NewTable())
		cb := L.CheckFunction(3)
		resolved, err := h.resolveJailed(path, h.writeJails())
		if err != nil {
			L.RaiseError("zemi.ui.edit: %v", err)
		}
		h.luaGate(L, "lua.fs.read", resolved)
		line := 0
		if n, ok := opts.RawGetString("line").(glua.LNumber); ok {
			line = int(n)
		}
		title := rawString(opts, "title")
		if title == "" {
			title = filepath.Base(resolved)
		}
		at := h.callerOf(L)
		ui := h.hooks()
		h.GoSafe("ui.edit", func() {
			if ui == nil {
				h.callLuaAsync(at, cb, "no UI in this run")
				return
			}
			h.lspDirty()
			if err := ui.EditFile(context.Background(), EditRequest{Path: resolved, Line: line, Title: title}); err != nil {
				h.callLuaAsync(at, cb, err.Error())
				return
			}
			h.callLuaAsync(at, cb)
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
		text := L.CheckString(1)
		width := L.CheckInt(2)
		out := L.NewTable()
		for _, line := range h.Markdown(text, width) {
			row := L.NewTable()
			for _, s := range line {
				seg := L.NewTable()
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
				row.Append(seg)
			}
			out.Append(row)
		}
		L.Push(out)
		return 1
	}))

	hl := L.NewTable()
	// hl.set(group, spec) changes one highlight group on top of the active
	// theme (and any theme set later): { fg, bg, sp, bold, italic, underline,
	// reverse, brighten, replace }, colors as hex or palette names, or
	// { link = "Group" }. theme.add builds a whole theme.
	hl.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.ui.hl.set")
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
		case "none":
			out["underline"] = false
		case "single":
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
		h.refuseDuringRender(L, "zemi.ui.reasoning.toggle")
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
		h.refuseDuringRender(L, "zemi.ui.redraw")
		h.refresh()
		return 0
	}))

	// title(text?) sets the terminal tab's title; nil gives it back to the
	// session. A reload that does not set it again gives it back too.
	tb.RawSetString("title", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.ui.title")
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
		h.refuseDuringRender(L, "zemi.ui.help")
		h.OpenHelp()
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

// checkColor refuses a color that can never resolve. A name outside the
// palette used to draw as nothing, with no error anywhere, and a CSS color
// name is the first thing anyone writes (DESIGN §6.0).
// ANSIColors are the terminal's own colors: a group names one instead of a
// hex value to let the user's terminal palette decide it.
var ANSIColors = map[string]ansi.BasicColor{
	"black": ansi.Black, "red": ansi.Red, "green": ansi.Green, "yellow": ansi.Yellow,
	"blue": ansi.Blue, "magenta": ansi.Magenta, "cyan": ansi.Cyan, "white": ansi.White,
}

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
		spec.Underline = map[bool]string{true: "single", false: "none"}[u]
	case string:
		if !slices.Contains(UnderlineKinds, u) {
			L.RaiseError("%s: underline %q is not one of %s", where, u, strings.Join(UnderlineKinds, ", "))
		}
		spec.Underline = u
	default:
		L.RaiseError("%s: underline must be a boolean or a string", where)
	}
	if b, ok := m["brighten"].(float64); ok {
		spec.Brighten = max(-1, min(b, 1))
	}
	return spec
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

// luaReload is a Lua overlay's reload: fn() returns the new items, which
// render_item renders again, under the overlay's origin. A failure or a
// stale generation keeps the items the overlay has.
func (h *Host) luaReload(o Overlay, fn, render *glua.LFunction) func() ([]any, [][]Segment) {
	return func() ([]any, [][]Segment) {
		items, text := o.Items, o.ItemText
		_, err := h.RT.Do(context.Background(), lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
			if h.staleOverlay(o) {
				return nil, lua.ErrReloaded
			}
			defer h.RT.PushFrame(L, o.Origin, o.Path)()
			base := L.GetTop()
			defer L.SetTop(base)
			L.Push(fn)
			if err := L.PCall(0, 1, nil); err != nil {
				return nil, err
			}
			v, err := lua.ToGo(L.Get(-1))
			if err != nil {
				return nil, err
			}
			list, ok := v.([]any)
			if m, isMap := v.(map[string]any); isMap && len(m) == 0 {
				list, ok = nil, true // Lua's {} marshals as an empty map
			}
			if !ok {
				return nil, fmt.Errorf("reload must return a list of items, got %T", v)
			}
			items, text = list, nil
			if render != nil {
				text = h.renderItems(L, o.ID, render, list)
			}
			return nil, nil
		})
		if err != nil && !isQuietLuaErr(err) {
			h.RecordError("overlay "+o.ID+" reload", err.Error())
		}
		return items, text
	}
}

// cancelWith is an overlay on_close that calls cb(nil), the vim.ui cancel shape.
func cancelWith(L *glua.LState, cb *glua.LFunction) *glua.LFunction {
	return L.NewFunction(func(L *glua.LState) int {
		L.Push(cb)
		L.Push(glua.LNil)
		L.Call(1, 0)
		return 0
	})
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

// ActiveThemeName returns the configured theme.
func (h *Host) ActiveThemeName() string {
	return cmp.Or(h.Settings.Str("ui.theme"), "default")
}
