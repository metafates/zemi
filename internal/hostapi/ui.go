package hostapi

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/metafates/zemi/internal/lua"
	glua "github.com/yuin/gopher-lua"
)

func (h *Host) uiNamespace(L *glua.LState, sb *lua.Sandbox) *glua.LTable {
	tb := L.NewTable()

	widget := L.NewTable()
	widget.RawSetString("register", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.ui.widget.register")
		tb, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.ui.widget.register: expected a spec table")
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
			L.RaiseError("zemi.ui.widget.register: %v", err)
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
	theme.RawSetString("register", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.ui.theme.register")
		name := L.CheckString(1)
		spec := luaToGoChecked(L, 2, "zemi.ui.theme.register")
		m, _ := spec.(map[string]any)
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
				if s, ok := v.(string); ok {
					t.Palette[k] = s
				}
			}
		}
		if hl, ok := m["hl"].(map[string]any); ok {
			for k, v := range hl {
				if spec, ok := v.(map[string]any); ok {
					t.HL[k] = hlSpec(spec)
				}
			}
		}
		if _, err := h.Themes.Add(t); err != nil {
			L.RaiseError("zemi.ui.theme.register: %v", err)
		}
		return 0
	}))
	theme.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.ui.theme.set")
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
			Empty:  rawString(tb, "empty"),
			Origin: h.RT.CurrentOrigin(L),
			Path:   h.RT.CurrentPath(L),
			Gen:    h.currentGen(),
			Anchor: rawString(tb, "anchor"),
		}
		if o.Anchor != "" && o.Anchor != "bottom" {
			L.RaiseError("zemi.ui.overlay.open: anchor must be \"bottom\" or nil, got %q", o.Anchor)
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
		if fn, ok := tb.RawGetString("on_close").(*glua.LFunction); ok {
			o.OnClose = fn
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
			ui.CloseOverlay(id)
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
		list, _ := items.([]any)
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
	// on_done(mode) once it closes, or on_done(nil, err). Never blocks the
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
				h.callLuaAsync(at, cb, nil, "no UI in this run")
				return
			}
			mode, err := ui.EditFile(context.Background(), EditRequest{Path: resolved, Line: line, Title: title})
			if err != nil {
				h.callLuaAsync(at, cb, nil, err.Error())
				return
			}
			h.callLuaAsync(at, cb, mode)
		})
		return 0
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
				row.Append(seg)
			}
			out.Append(row)
		}
		L.Push(out)
		return 1
	}))

	// hl(group, spec) changes one highlight group on top of the active theme
	// (and any theme set later): { fg, bg, bold, italic, underline, reverse },
	// colors as hex or palette names, or { link = "Group" }. theme.register
	// builds a whole theme.
	tb.RawSetString("hl", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.ui.hl")
		group := L.CheckString(1)
		v := luaToGoChecked(L, 2, "zemi.ui.hl")
		m, _ := v.(map[string]any)
		h.Themes.SetHL(group, hlSpec(m))
		h.refresh()
		return 0
	}))

	// hl_get(group) is how the group draws under the active theme, links
	// followed and palette names resolved to hex, or nil when nothing sets it.
	tb.RawSetString("hl_get", L.NewFunction(func(L *glua.LState) int {
		theme := h.Themes.Get(h.ActiveThemeName())
		spec, ok := h.Themes.Group(theme, L.CheckString(1))
		if !ok {
			L.Push(glua.LNil)
			return 1
		}
		color := func(ref string) any {
			if theme != nil && ref != "" && !strings.HasPrefix(ref, "#") {
				ref = theme.Palette[ref]
			}
			if ref == "" {
				return nil
			}
			return ref
		}
		L.Push(lua.FromGo(L, map[string]any{
			"fg": color(spec.Fg), "bg": color(spec.Bg), "bold": spec.Bold, "italic": spec.Italic,
			"underline": spec.Underline, "reverse": spec.Reverse,
		}))
		return 1
	}))

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

	tb.RawSetString("help", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.ui.help")
		h.OpenHelp()
		return 0
	}))

	sb.FreezeTable(widget, "namespace zemi.ui.widget")
	sb.FreezeTable(theme, "namespace zemi.ui.theme")
	sb.FreezeTable(overlay, "namespace zemi.ui.overlay")
	sb.FreezeTable(focus, "namespace zemi.ui.focus")

	return tb
}

func hlSpec(m map[string]any) HLSpec {
	return HLSpec{
		Link: strField(m, "link"),
		Fg:   strField(m, "fg"), Bg: strField(m, "bg"),
		Bold: boolField(m, "bold"), Italic: boolField(m, "italic"),
		Underline: boolField(m, "underline"), Reverse: boolField(m, "reverse"),
	}
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
	if v, ok := h.Settings.Get("ui.theme"); ok {
		if s, ok := v.(string); ok && !strings.HasPrefix(s, "!") {
			return s
		}
	}
	return "default"
}
