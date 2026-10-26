package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
)

// uiAsk opens the ask o and returns a handle: wait() suspends the task until
// the user answers with the value the callback gets, nil on cancel;
// cancel() closes the overlay, which answers nil. The callback itself runs
// through the overlay's on_select/on_close as before. With no UI the ask
// fails closed: the callback gets nil once and /errors names the call, like
// an ask nobody can answer denies.
func (h *Host) uiAsk(L *glua.LState, api string, o Overlay, cb *glua.LFunction) int {
	done := make(chan struct{})
	var ans any
	o.Answered = func(v any) { ans = v; close(done) }
	if ui := h.hooks(); ui != nil {
		ui.OpenOverlay(o)
	} else {
		h.RecordError(api, "", "no UI is running to answer it")
		o.Answered(nil)
		// off the interpreter: a task here would wait on the queue it sits in
		at := h.callerOf(L)
		h.GoSafe(api, func() { h.callLuaAsync(api, at, cb, nil) })
	}
	handle := L.NewTable()
	handle.RawSetString("wait", lua.Suspending(L, L.NewFunction(func(L *glua.LState) int {
		return waitHandle(L, strings.TrimPrefix(api, "zemi.ui.")+":wait", done, time.Hour, nil, func(L *glua.LState, err error) []glua.LValue {
			if err != nil {
				return []glua.LValue{glua.LNil, glua.LString(err.Error())}
			}
			if ans == nil {
				return []glua.LValue{glua.LNil}
			}
			return []glua.LValue{lua.FromGo(L, ans)}
		})
	})))
	handle.RawSetString("cancel", L.NewFunction(func(L *glua.LState) int {
		if ui := h.hooks(); ui != nil {
			ui.CloseOverlay(o.ID)
		}
		return 0
	}))
	lua.GuardFuncs(L, handle, strings.TrimPrefix(api, "zemi.ui.")+":", h.guardLua)
	L.Push(handle)
	return 1
}

func (h *Host) uiNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	widget := L.NewTable()
	widget.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		tb, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.ui.widget.add: expected a spec table")
		}
		onlyTableKeys(L, tb, "zemi.ui.widget.add", "id", "region", "priority", "wrap", "render", "on_click")
		w := &Widget{
			ID:     rawString(L, tb, "id"),
			Region: rawString(L, tb, "region"),
			Wrap:   rawBool(L, tb, "wrap"),
			Origin: h.RT.CurrentOrigin(L),
			Path:   h.RT.CurrentPath(L),
			Gen:    h.currentGen(),
		}
		if n, ok := opt[glua.LNumber](L, tb, "priority"); ok {
			w.Priority = float64(n)
		}
		w.Render, _ = opt[*glua.LFunction](L, tb, "render")
		w.OnClick, _ = opt[*glua.LFunction](L, tb, "on_click")
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
		var rows []any
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
		p, err := parsePalette(tableArg(L, 2, "zemi.ui.theme.add"))
		if err != nil {
			L.RaiseError("zemi.ui.theme.add: %v", err)
		}
		t := &Theme{Name: name, Palette: p, Origin: h.RT.CurrentOrigin(L), Path: h.RT.CurrentPath(L), Gen: h.currentGen()}
		if _, err := h.Themes.Add(t); err != nil {
			L.RaiseError("zemi.ui.theme.add: %v", err)
		}
		return 0
	}))
	theme.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		name := ""
		if t := h.ActiveTheme(); t != nil {
			name = t.Name
		}
		L.Push(glua.LString(name))
		return 1
	}))
	theme.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		var rows []any
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
		onlyTableKeys(L, tb, "zemi.ui.overlay.open", "id", "title", "body", "markdown", "empty", "anchor", "follow",
			"selected", "items", "render_item", "reload", "on_select", "on_action", "on_close", "keys")
		o := Overlay{
			ID:     rawString(L, tb, "id"),
			Title:  rawString(L, tb, "title"),
			BodyMD: rawBool(L, tb, "markdown"),
			Empty:  rawString(L, tb, "empty"),
			Origin: h.RT.CurrentOrigin(L),
			Path:   h.RT.CurrentPath(L),
			Gen:    h.currentGen(),
			Anchor: OverlayAnchor(rawString(L, tb, "anchor")),
			Follow: rawBool(L, tb, "follow"),
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
			h.renderItems(L, o.ID, render, o.Items)
		}
		if fn, ok := opt[*glua.LFunction](L, tb, "reload"); ok {
			o.Reload = h.luaReload(o, fn, render)
		}
		o.OnSelect, _ = opt[*glua.LFunction](L, tb, "on_select")
		o.OnAction, _ = opt[*glua.LFunction](L, tb, "on_action")
		o.OnClose, _ = opt[*glua.LFunction](L, tb, "on_close")
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
			ui.CloseOverlay(id)
		}
		return 0
	}))
	tb.RawSetString("overlay", overlay)

	// select(items, opts, on_choice): a picker in the shape of vim.ui.select
	// over the overlay stack. on_choice(item) runs on enter, on_choice(nil)
	// on cancel; opts.prompt titles it, opts.format_item renders rows. The
	// handle's wait() gives the same answer to a task.
	tb.RawSetString("select", L.NewFunction(func(L *glua.LState) int {
		itemsV := L.CheckTable(1)
		opts := L.OptTable(2, L.NewTable())
		cb := L.CheckFunction(3)
		items, err := lua.ToGo(itemsV)
		if err != nil {
			L.RaiseError("zemi.ui.select: %v", err)
		}
		list, ok := luaList(items)
		if !ok {
			L.RaiseError("zemi.ui.select: items must be a list; the order is select(items, opts, on_choice)")
		}
		o := Overlay{
			ID:       fmt.Sprintf("select:%d", h.selectSeq.Add(1)),
			Title:    rawString(L, opts, "prompt"),
			Origin:   h.RT.CurrentOrigin(L),
			Path:     h.RT.CurrentPath(L),
			Gen:      h.currentGen(),
			OnSelect: cb,
			OnClose:  cb, // cb() gets nil, the vim.ui cancel shape
		}
		if list != nil { // an empty picker is no list to parse
			if o.Items, err = overlayItems(items); err != nil {
				L.RaiseError("zemi.ui.select: %v", err)
			}
		}
		if fn, ok := opt[*glua.LFunction](L, opts, "format_item"); ok {
			h.renderItems(L, o.ID, fn, o.Items)
		}
		return h.uiAsk(L, "zemi.ui.select", o, cb)
	}))

	// input(opts, on_confirm): a one-line prompt in the shape of
	// vim.ui.input. on_confirm(text) runs on enter, on_confirm(nil) on esc;
	// opts.prompt titles it, opts.default prefills it, opts.placeholder
	// shows dim in the empty line. The handle's wait() gives the same answer.
	tb.RawSetString("input", L.NewFunction(func(L *glua.LState) int {
		opts := L.OptTable(1, L.NewTable())
		cb := L.CheckFunction(2)
		o := Overlay{
			ID:          fmt.Sprintf("input:%d", h.selectSeq.Add(1)),
			Title:       rawString(L, opts, "prompt"),
			Origin:      h.RT.CurrentOrigin(L),
			Path:        h.RT.CurrentPath(L),
			Gen:         h.currentGen(),
			Input:       true,
			Default:     rawString(L, opts, "default"),
			Placeholder: rawString(L, opts, "placeholder"),
			OnSelect:    cb,
			OnClose:     cb,
		}
		return h.uiAsk(L, "zemi.ui.input", o, cb)
	}))

	// edit(path, opts, on_done): the user's editor on a file, then
	// on_done() once it closes, or on_done(err). Never blocks the
	// interpreter. The gate is a read: the writing is the user's own.
	tb.RawSetString("edit", L.NewFunction(func(L *glua.LState) int {
		path := L.CheckString(1)
		opts := L.OptTable(2, L.NewTable())
		cb := L.CheckFunction(3)
		// the editor opens the path by name and follows symlinks anyway
		p, err := h.projectResolve(path, false)
		if err != nil {
			L.RaiseError("zemi.ui.edit: %v", err)
		}
		resolved := p.String()
		// gated by the written name like zemi.fs, so a secret-named link asks
		h.luaGate(L, luaFSRead, h.projectAbs(perm.ExpandHome(path)), nil)
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
			if err := ui.EditFile(context.Background(), EditRequest{Path: resolved, Line: line, Title: title}); err != nil {
				h.callLuaAsync("zemi.ui.edit on_done", at, cb, err.Error())
				return
			}
			h.lspDirty()
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

	marks := L.CreateTable(0, len(Marks))
	for k, m := range Marks {
		mt := L.CreateTable(0, 2)
		mt.RawSetString("glyph", glua.LString(m.Glyph))
		mt.RawSetString("hl", glua.LString(m.HL))
		marks.RawSetString(string(k), mt)
	}
	tb.RawSetString("marks", marks)

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
	// hl.set(group, spec) defines one highlight group for every theme:
	// { fg, bg, sp, bold, italic, underline, reverse, brighten, replace },
	// colors as palette roles, hex or ANSI names, or { link = "Group" }.
	hl.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		group := L.CheckString(1)
		h.Themes.SetHL(group, hlSpec(L, "zemi.ui.hl.set", tableArg(L, 2, "zemi.ui.hl.set")))
		h.refresh()
		return 0
	}))

	// hl.get(group) is how the group draws under the active theme, links
	// followed and roles resolved to hex (an ANSI color name is the
	// terminal's, so it stays a name), or nil when nothing sets it.
	hl.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		theme := h.ActiveTheme()
		spec, ok := h.Themes.Group(L.CheckString(1))
		if !ok {
			L.Push(glua.LNil)
			return 1
		}
		color := func(ref string) any {
			if _, ok := ANSIColors[ref]; !ok && ref != "NONE" {
				ref = Hex(theme.Color(ref))
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
		if spec.Shade != nil {
			out["shade"] = *spec.Shade
		}
		if spec.Replace {
			out["replace"] = true
		}
		L.Push(lua.FromGo(L, out))
		return 1
	}))
	tb.RawSetString("hl", hl)

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
		_ = h.OpenHelp()
		return 0
	}))

	return tb
}

// openInEditor opens path in the user's editor off the caller's goroutine:
// the editor blocks, and a Go command runs on the key loop.
func (h *Host) openInEditor(ui UIHooks, path, title string) {
	h.GoSafe("open "+title, func() {
		if err := ui.EditFile(context.Background(), EditRequest{Path: path, Title: title}); err != nil {
			h.Notify(slog.LevelError, "Could not open "+title, err.Error())
		}
	})
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
// roles would draw as nothing, and a CSS color name is the first thing
// anyone writes (DESIGN §6.0).
func checkColor(L *glua.LState, where, field, ref string) {
	_, isANSI := ANSIColors[ref]
	switch {
	case ref == "" || ref == "NONE" || isANSI || slices.Contains(Roles, ref):
	case strings.HasPrefix(ref, "#") && isHexColor(ref):
	case strings.HasPrefix(ref, "#"):
		L.RaiseError("%s: %s %q is not a hex color; write #rrggbb", where, field, ref)
	default:
		L.RaiseError("%s: %s %q is not #rrggbb, an ANSI color name, or a palette role (%s)",
			where, field, ref, strings.Join(Roles, ", "))
	}
}

// hlSpec reads a group table; misuse raises (DESIGN §6.0).
func hlSpec(L *glua.LState, where string, m map[string]any) HLSpec {
	flag := func(key string) *bool {
		if b, ok := m[key].(bool); ok {
			return &b
		}
		return nil
	}
	for _, f := range []string{"fg", "bg", "sp"} {
		checkColor(L, where, f, strField(m, f))
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
	if b, ok := m["shade"].(float64); ok {
		if strField(m, "fg") != "" {
			L.RaiseError("%s: shade and fg cannot combine", where)
		}
		s := max(0, min(b, 1))
		spec.Shade = &s
	}
	return spec
}

// Verb says what enter does on a row, in one word for the footer hint.
type Verb string

const (
	VerbOpen   Verb = "open"   // a screen of its own, over this list
	VerbPick   Verb = "pick"   // a value
	VerbRun    Verb = "run"    // a command
	VerbToggle Verb = "toggle" // a toggle list's row
	VerbFill   Verb = "fill"   // the composer
	VerbCopy   Verb = "copy"   // the row's value
	VerbGo     Verb = "go"     // a browsing list's leaf: close it, keeping the place
	VerbStop   Verb = "stop"   // the screen's own thing stops: a loop, a goal
)

var verbs = []Verb{VerbOpen, VerbPick, VerbRun, VerbToggle, VerbFill, VerbCopy, VerbGo, VerbStop}

// OverlayItem is an overlay row's fields, parsed from its item: a table of
// them, else a value whose text is the row. Callbacks get the raw item.
type OverlayItem struct {
	Text, Desc, Key, Section, Detail, Fill, Copy, Run, ID, Preview string
	Verb                                                           Verb
	Mark                                                           MarkKind   // drawn ahead of the text
	On, Locked                                                     *bool      // On: the switch is drawn, on or off; Locked: it stands with nothing to switch
	Danger                                                         bool       // cannot be undone: drawn in Danger, and its first enter or key only arms it
	Tail                                                           bool       // a text too wide is cut from the left: a path's name is at its end
	Anchor                                                         int        // a following list's row: the transcript row it stands for, 0 for none
	Keys                                                           []ItemKey  // what acts on the row: from the list by its character, in the footer of its view
	Fields                                                         []Field    // the row's view: facts set above Detail
	Parts                                                          []BodyPart // the row's view's body in Detail's place, each drawn the way its text is written
	Empty                                                          string     // the row's view's pending line, under the body while the work runs
	Row                                                            []Segment  // the row render_item drew, nil for the text alone
	// raw is the Lua value the row was parsed from, which its callbacks get
	// back: a Go screen's row has none.
	raw any
}

// ItemKey is one operation a row offers: a character that runs it from the
// list and names it in the footer of the row's view.
type ItemKey struct {
	Key, Desc string
	Run, Fill string
	ID        string // hands it to the overlay's on_action
	Verb      Verb   // VerbOpen: what it opens sits over the list, which stays; VerbGo: it takes the chat, so even a toggle list closes
	Danger    bool   // acts on its second press, like a list key
	Raw       any    // what on_action gets
}

// Field is a name and value a detail view sets as a table above its text.
type Field struct{ Name, Value string }

// BodyPart is one part of a view's body, drawn in the form its text is
// written in: markdown, a unified diff ("diff"), a language to highlight it
// as, or "" for plain text. A part with a Fill joins the well its neighbours
// with one make: a padding row above each part, one below the last, and each
// on its own surface.
type BodyPart struct {
	Text string
	Lang string
	Fill Fill
}

// fieldList is a row's view fields from name, value pairs.
func fieldList(kv ...string) []Field {
	if len(kv)%2 != 0 {
		panic("fieldList: a name without a value")
	}
	out := make([]Field, 0, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		out = append(out, Field{kv[i], kv[i+1]})
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
		return OverlayItem{Text: s, raw: item}, nil
	}
	p := OverlayItem{raw: item}
	var err error
	bad := func(key, want string, v any) {
		if err == nil {
			err = fmt.Errorf("%s must be %s, got %s", key, want, luaShape(v))
		}
	}
	for _, f := range []struct {
		key string
		dst *string
	}{{"text", &p.Text}, {"desc", &p.Desc}, {"key", &p.Key}, {"section", &p.Section}, {"detail", &p.Detail}, {"fill", &p.Fill}, {"copy", &p.Copy}, {"run", &p.Run}, {"id", &p.ID}, {"preview", &p.Preview}} {
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
	switch v := m["verb"].(type) {
	case nil:
	case Verb: // a Go screen's item
		if p.Verb = v; !slices.Contains(verbs, p.Verb) {
			bad("verb", "one of "+joinVerbs(), v)
		}
	case string:
		if p.Verb = Verb(v); !slices.Contains(verbs, p.Verb) {
			bad("verb", "one of "+joinVerbs(), v)
		}
	default:
		bad("verb", "a string", v)
	}
	switch v := m["mark"].(type) {
	case nil:
	case MarkKind: // a Go screen's item
		p.Mark = v
	case string:
		p.Mark = MarkKind(v)
	default:
		bad("mark", "a zemi.ui.marks key", v)
	}
	if _, ok := Marks[p.Mark]; !ok && p.Mark != "" {
		bad("mark", "a zemi.ui.marks key", p.Mark)
		p.Mark = ""
	}
	for _, f := range []struct {
		key string
		dst **bool // nil is no switch, false a switch drawn off
	}{{"on", &p.On}, {"locked", &p.Locked}} {
		switch v := m[f.key].(type) {
		case nil:
		case bool:
			*f.dst = &v
		default:
			bad(f.key, "a boolean", v)
		}
	}
	for _, f := range []struct {
		key string
		dst *bool
	}{{"danger", &p.Danger}, {"tail", &p.Tail}} {
		switch v := m[f.key].(type) {
		case nil:
		case bool:
			*f.dst = v
		default:
			bad(f.key, "a boolean", v)
		}
	}
	switch v := m["anchor"].(type) {
	case nil:
	case float64:
		p.Anchor = int(v)
	case int:
		p.Anchor = v
	default:
		bad("anchor", "a number", v)
	}
	fields, ok := luaList(m["fields"])
	for _, f := range fields {
		pair, _ := f.([]any)
		var name, value string
		if ok = len(pair) == 2; ok {
			name, ok = pair[0].(string)
		}
		if ok {
			value, ok = pair[1].(string)
		}
		if !ok {
			break
		}
		p.Fields = append(p.Fields, Field{name, value})
	}
	if !ok {
		bad("fields", "a list of { name, value } pairs", m["fields"])
	}
	switch v := m["keys"].(type) {
	case nil:
	case []any:
		for i, k := range v {
			ik, e := parseKey(k)
			if e != nil && err == nil {
				err = fmt.Errorf("keys[%d].%w", i+1, e)
			}
			// a key runs one row action: a second on its letter is never read
			if slices.ContainsFunc(p.Keys, func(prev ItemKey) bool { return prev.Key == ik.Key }) {
				e = fmt.Errorf("key %q is another of the row's", ik.Key)
				if err == nil {
					err = fmt.Errorf("keys[%d].%w", i+1, e)
				}
			}
			p.Keys = append(p.Keys, ik)
		}
	case map[string]any:
		if len(v) > 0 {
			bad("keys", "a list", v)
		} // Lua's {} marshals as an empty map
	default:
		bad("keys", "a list", v)
	}
	return p, err
}

// parseKey reads one of a row's keys.
func parseKey(v any) (ItemKey, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return ItemKey{}, fmt.Errorf("a key is a table, got %s", luaShape(v))
	}
	var k ItemKey
	k.Raw = v
	var err error
	for _, f := range []struct {
		name string
		dst  *string
	}{{"key", &k.Key}, {"desc", &k.Desc}, {"run", &k.Run}, {"fill", &k.Fill}, {"id", &k.ID}} {
		switch x := m[f.name].(type) {
		case nil:
		case string:
			*f.dst = x
		default:
			if err == nil {
				err = fmt.Errorf("%s must be a string, got %s", f.name, luaShape(x))
			}
		}
	}
	if d, ok := m["danger"].(bool); ok {
		k.Danger = d
	}
	if v, ok := m["verb"].(string); ok {
		k.Verb = Verb(v)
		if !slices.Contains(verbs, k.Verb) {
			err = fmt.Errorf("verb must be one of %s, got %q", joinVerbs(), v)
		}
	}
	switch {
	case k.Key == "":
		err = errors.New("key is the character it answers to")
	case utf8.RuneCountInString(k.Key) != 1 || strings.Contains(listOwnKeys, k.Key):
		err = fmt.Errorf("key must be one character other than j, k, /, y and Y, got %q", k.Key)
	}
	return k, err
}

// joinVerbs lists the verbs for an error message.
func joinVerbs() string {
	names := make([]string, len(verbs))
	for i, v := range verbs {
		names[i] = string(v)
	}
	return strings.Join(names, ", ")
}

// overlayItems is a Lua items list with every item's fields checked. nil is
// no list, so a reload that returns nothing keeps the old items.
func overlayItems(v any) ([]OverlayItem, error) {
	list, ok := luaList(v)
	if !ok || v == nil {
		return nil, fmt.Errorf("items must be a list, got %s", luaShape(v))
	}
	items := make([]OverlayItem, len(list))
	for i, item := range list {
		p, err := ParseOverlayItem(item)
		if err != nil {
			return nil, fmt.Errorf("items[%d].%w", i+1, err)
		}
		items[i] = p
	}
	return items, nil
}

// renderItems runs an overlay's render_item here on the interpreter, so the
// UI goroutine never waits on the queue to draw the rows. render_item
// returns a string or one line of segments; a failing item falls back to its
// plain value.
func (h *Host) renderItems(L *glua.LState, id string, fn *glua.LFunction, items []OverlayItem) {
	for i := range items {
		item := &items[i]
		rets, err := h.callFn(L, h.callerOf(L), fn, 1, lua.FromGo(L, item.raw))
		if err == nil {
			switch v := rets[0].(type) {
			case *glua.LTable:
				var line any
				if line, err = lua.ToGo(v); err == nil {
					var rows [][]Segment
					if rows, err = convertSegments([]any{line}, ""); err == nil {
						item.Row = rows[0]
					}
				}
			case *glua.LNilType:
			default:
				item.Row = []Segment{{Text: v.String()}}
			}
		}
		if err != nil {
			h.Log.Debug("overlay render_item failed", "origin", "ui", "id", id, "error", err)
			item.Row = []Segment{{Text: fmt.Sprint(item.raw)}}
		}
	}
}

// luaReload is a Lua overlay's reload: fn(), a task, returns the new items,
// which render_item renders again, under the overlay's origin. A failure or a
// stale generation keeps the items the overlay has.
func (h *Host) luaReload(o Overlay, fn, render *glua.LFunction) func() []OverlayItem {
	return func() []OverlayItem {
		items := o.Items
		t := h.luaTask(lua.ClassUI, luaCaller{gen: o.Gen, origin: o.Origin, path: o.Path}, fn)
		t.Done = func(L *glua.LState, rets []glua.LValue) ([]any, error) {
			v, err := lua.ToGo(rets[0])
			var list []OverlayItem
			if err == nil {
				list, err = overlayItems(v)
			}
			if err != nil {
				return nil, err
			}
			if render != nil {
				h.renderItems(L, o.ID, render, list)
			}
			items = list
			return nil, nil
		}
		_, err := h.RT.RunTask(context.Background(), t)
		if err != nil && !lua.Dropped(err) {
			h.RecordError("overlay", o.ID+" reload", err.Error())
		}
		return items
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
		_, err := h.RT.Do(context.Background(), lua.ClassUI, 0, 0, "hook "+name, func(L *glua.LState) ([]any, error) {
			if h.currentGen() != gen {
				return nil, nil
			}
			h.emitSync(L, h.Events.Handlers(name), name, payload, "host")
			return nil, nil
		})
		h.refresh()
		if err != nil {
			h.Log.Debug("async emit dropped", "origin", "events", "event", name, "error", err)
		}
	})
}

// checkTheme refuses a ui.theme value no theme answers to.
func (h *Host) checkTheme(name string) error {
	if h.Themes.Get(name) != nil {
		return nil
	}
	var names []string
	for _, t := range h.Themes.List() {
		names = append(names, t.Name)
	}
	return Usagef("unknown theme %q: themes are %s", name, strings.Join(names, ", "))
}

// CheckTheme records a ui.theme no theme answers to once boot is done: a
// typo, or a pick of a removed plugin's theme, draws whorl instead.
func (h *Host) CheckTheme() {
	if name := h.Settings.Str(config.KeyUITheme); h.Themes.Get(name) == nil {
		h.RecordErrorOnce("ui.theme", "", fmt.Sprintf("ui.theme %q names no theme, so whorl draws; /theme lists them", name))
	}
}

// ActiveTheme is the theme drawing now: the configured one, whorl when
// that names none, and its light twin on a light terminal. nil before any
// theme is.
func (h *Host) ActiveTheme() *Theme {
	t := h.Themes.Get(h.Settings.Str(config.KeyUITheme))
	if t == nil {
		t = h.Themes.Get("whorl")
	}
	if t == nil || !h.lightTerm.Load() {
		return t
	}
	return cmp.Or(h.Themes.Light(t), t)
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
			switch s.Pin {
			case PinNone:
			case PinTurn:
				seg.RawSetString("sticky", glua.LTrue)
			case PinSection:
				seg.RawSetString("sticky", glua.LString("section"))
			case PinEnd:
				seg.RawSetString("sticky", glua.LString("end"))
			}
			if s.Click != "" {
				seg.RawSetString("click", glua.LString(s.Click))
			}
			if s.Link != "" {
				seg.RawSetString("link", glua.LString(s.Link))
			}
			row.Append(seg)
		}
		out.Append(row)
	}
	return out
}
