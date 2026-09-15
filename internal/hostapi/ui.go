package hostapi

import (
	"context"
	"fmt"
	"strings"

	"github.com/metafates/zemi/internal/lua"
	glua "github.com/yuin/gopher-lua"
)

func (h *Host) uiNamespace(L *glua.LState, sb *lua.Sandbox) *glua.LTable {
	tb := L.NewTable()

	widget := L.NewTable()
	widget.RawSetString("register", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.ui.widget.register") {
			return 0
		}
		tb, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.ui.widget.register: expected a spec table")
		}
		w := &Widget{
			ID:     rawString(tb, "id"),
			Region: rawString(tb, "region"),
			Wrap:   rawBool(tb, "wrap"),
			Origin: h.RT.CurrentOrigin(),
			Path:   h.RT.CurrentPath(),
			Gen:    h.currentGen(),
		}
		if n, ok := tb.RawGetString("priority").(glua.LNumber); ok {
			w.Priority = int(n)
		}
		if fn, ok := tb.RawGetString("render").(*glua.LFunction); ok {
			w.Render = fn
		}
		if fn, ok := tb.RawGetString("keys").(*glua.LFunction); ok {
			w.Keys = fn
		}
		if _, err := h.Widgets.Add(w); err != nil {
			L.RaiseError("zemi.ui.widget.register: %v", err)
		}
		h.refresh()
		return 0
	}))
	widget.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		for _, w := range h.Widgets.List() {
			row := L.NewTable()
			row.RawSetString("id", glua.LString(w.ID))
			row.RawSetString("region", glua.LString(w.Region))
			row.RawSetString("priority", glua.LNumber(w.Priority))
			row.RawSetString("origin", glua.LString(w.Origin))
			out.Append(row)
		}
		L.Push(out)
		return 1
	}))
	tb.RawSetString("widget", widget)

	theme := L.NewTable()
	theme.RawSetString("register", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.ui.theme.register") {
			return 0
		}
		name := L.CheckString(1)
		spec, err := luaToGoChecked(L, 2, "zemi.ui.theme.register")
		if err != nil {
			return 0
		}
		m, _ := spec.(map[string]any)
		t := &Theme{
			Name:    name,
			Palette: map[string]string{},
			HL:      map[string]HLSpec{},
			Origin:  h.RT.CurrentOrigin(),
			Path:    h.RT.CurrentPath(),
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
				spec, ok := v.(map[string]any)
				if !ok {
					continue
				}
				t.HL[k] = HLSpec{
					Fg:        strField(spec, "fg"),
					Bg:        strField(spec, "bg"),
					Bold:      boolField(spec, "bold"),
					Italic:    boolField(spec, "italic"),
					Underline: boolField(spec, "underline"),
					Reverse:   boolField(spec, "reverse"),
				}
			}
		}
		if _, err := h.Themes.Add(t); err != nil {
			L.RaiseError("zemi.ui.theme.register: %v", err)
		}
		return 0
	}))
	theme.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.ui.theme.set") {
			return 0
		}
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
		name := "default"
		if v, ok := h.Settings.Get("ui.theme"); ok {
			if s, ok := v.(string); ok {
				name = s
			}
		}
		L.Push(glua.LString(name))
		return 1
	}))
	tb.RawSetString("theme", theme)

	overlay := L.NewTable()
	overlay.RawSetString("open", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.ui.overlay.open") {
			return 0
		}
		tb, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.ui.overlay.open: expected a spec table")
		}
		o := Overlay{
			ID:     rawString(tb, "id"),
			Title:  rawString(tb, "title"),
			Origin: h.RT.CurrentOrigin(),
			Path:   h.RT.CurrentPath(),
			Gen:    h.currentGen(),
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
		if fn, ok := tb.RawGetString("render_item").(*glua.LFunction); ok {
			o.RenderItem = fn
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
		if h.refuseDuringRender(L, "zemi.ui.overlay.close") {
			return 0
		}
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
		if h.refuseDuringRender(L, "zemi.ui.select") {
			return 0
		}
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
			Origin:   h.RT.CurrentOrigin(),
			Path:     h.RT.CurrentPath(),
			Gen:      h.currentGen(),
			Items:    list,
			OnSelect: cb,
			OnClose: L.NewFunction(func(L *glua.LState) int {
				L.Push(cb)
				L.Push(glua.LNil)
				L.Call(1, 0)
				return 0
			}),
		}
		if fn, ok := opts.RawGetString("format_item").(*glua.LFunction); ok {
			o.RenderItem = fn
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
		if h.refuseDuringRender(L, "zemi.ui.input") {
			return 0
		}
		opts := L.OptTable(1, L.NewTable())
		cb := L.CheckFunction(2)
		o := Overlay{
			ID:       fmt.Sprintf("input:%d", h.selectSeq.Add(1)),
			Title:    rawString(opts, "prompt"),
			Origin:   h.RT.CurrentOrigin(),
			Path:     h.RT.CurrentPath(),
			Gen:      h.currentGen(),
			Input:    true,
			Default:  rawString(opts, "default"),
			OnSelect: cb,
			OnClose: L.NewFunction(func(L *glua.LState) int {
				L.Push(cb)
				L.Push(glua.LNil)
				L.Call(1, 0)
				return 0
			}),
		}
		if ui := h.hooks(); ui != nil {
			ui.OpenOverlay(o)
		}
		return 0
	}))

	tb.RawSetString("invalidate", L.NewFunction(func(L *glua.LState) int {
		id := "all"
		if L.GetTop() >= 1 && L.Get(1) != glua.LNil {
			id = L.CheckString(1)
		}
		h.Log.Debug("ui", "invalidate requested (no-op in v1: full re-render per frame)", map[string]any{"id": id})
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
	// colors as hex or palette names. theme.register builds a whole theme.
	tb.RawSetString("hl", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.ui.hl") {
			return 0
		}
		group := L.CheckString(1)
		v, err := luaToGoChecked(L, 2, "zemi.ui.hl")
		if err != nil {
			return 0
		}
		m, _ := v.(map[string]any)
		h.Themes.SetHL(group, HLSpec{
			Fg: strField(m, "fg"), Bg: strField(m, "bg"),
			Bold: boolField(m, "bold"), Italic: boolField(m, "italic"),
			Underline: boolField(m, "underline"), Reverse: boolField(m, "reverse"),
		})
		h.refresh()
		return 0
	}))

	tb.RawSetString("help", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.ui.help") {
			return 0
		}
		h.OpenHelp()
		return 0
	}))

	sb.FreezeTable(widget, "namespace zemi.ui.widget")
	sb.FreezeTable(theme, "namespace zemi.ui.theme")
	sb.FreezeTable(overlay, "namespace zemi.ui.overlay")

	return tb
}

func boolField(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	b, _ := m[key].(bool)
	return b
}

// EmitAsync delivers a host event without blocking the caller: it is safe
// from the interpreter goroutine (a blocking Emit there would deadlock the
// queue). Stale-generation deliveries are dropped.
func (h *Host) EmitAsync(name string, payload map[string]any) {
	gen := h.currentGen()
	h.GoSafe("event "+name, func() {
		_, err := h.RT.Do(context.Background(), lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
			if h.currentGen() != gen {
				return nil, nil
			}
			h.emitSync(L, h.Events.Handlers(name), name, payload, "host")
			return nil, nil
		})
		if err != nil {
			h.Log.Debug("events", "async emit dropped", map[string]any{"event": name, "error": err.Error()})
		}
	})
}

// ActiveThemeName returns the configured theme (ui.go helper used by ui).
func (h *Host) ActiveThemeName() string {
	if v, ok := h.Settings.Get("ui.theme"); ok {
		if s, ok := v.(string); ok && !strings.HasPrefix(s, "!") {
			return s
		}
	}
	return "default"
}
