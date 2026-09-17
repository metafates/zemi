// win.go exposes windows (internal/win) to Lua as zemi.win and to the
// model as the window tool: a program running in a split or a float over
// the chat, driven and read back without leaving zemi. Windows are host
// state: they survive turns and config reloads; only Lua callbacks are
// generation-tagged.
package hostapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	uv "github.com/charmbracelet/ultraviolet"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sandbox"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
	"github.com/metafates/zemi/internal/win"
)

// openWindow starts a window and wires its exit to the event, the next
// model message and a redraw.
func (h *Host) openWindow(spec win.Spec) (*win.Window, error) {
	spec.Title = sanitize.String(spec.Title)
	if spec.Dir == "" {
		spec.Dir = h.Dirs.ProjectDir
	}
	if spec.Env == nil {
		// secret-shaped names stay out, as for bash and jobs: the screen is read back
		spec.Env, _ = perm.ChildEnv(nil)
	}
	return h.openedWindow(h.Wins.Open(spec))
}

// openTextWindow opens a window the UI renders from text: no program, so no
// directory, environment or sandbox.
func (h *Host) openTextWindow(spec win.Spec) (*win.Window, error) {
	spec.Title = sanitize.String(spec.Title)
	return h.openedWindow(h.Wins.OpenText(spec))
}

func (h *Host) openedWindow(w *win.Window, err error) (*win.Window, error) {
	if err != nil {
		return nil, err
	}
	h.EmitAsync("win.open", map[string]any{"id": w.ID, "cmd": w.Cmd, "kind": w.Kind(), "title": w.Title()})
	h.refresh()
	return w, nil
}

// windowExited runs on the window's wait goroutine (Manager.OnExit).
func (h *Host) windowExited(w *win.Window) {
	h.EmitAsync("win.exit", map[string]any{"id": w.ID, "cmd": w.Cmd, "code": w.ExitCode(), "title": w.Title()})
	if w.Owner == "model" {
		ag := h.agents
		ag.mu.Lock()
		ag.shellNotes = append(ag.shellNotes, fmt.Sprintf("\n<window id=%d title=%q exited code=%d/>", w.ID, w.Title(), w.ExitCode()))
		ag.mu.Unlock()
	}
	h.finishWinGuard(w)
	h.refresh()
}

// finishWinGuard runs a sandboxed window's guardSandbox check once, and its
// warning reaches the next message.
func (h *Host) finishWinGuard(w *win.Window) {
	f, ok := h.winGuards.LoadAndDelete(w.ID)
	if !ok {
		return
	}
	if note := f.(func() string)(); note != "" {
		ag := h.agents
		ag.mu.Lock()
		ag.shellNotes = append(ag.shellNotes, fmt.Sprintf("\n<window id=%d>%s</window>", w.ID, note))
		ag.mu.Unlock()
	}
}

// OpenWindow starts a window from the UI.
func (h *Host) OpenWindow(spec win.Spec) (*win.Window, error) { return h.openWindow(spec) }

type winArm struct {
	id    int
	until time.Time
}

// CloseWindow ends a window for the user (×, /win close). A running editor
// may hold unsaved changes, so the first close is refused with a warning and
// only a second one within 3s kills it.
func (h *Host) CloseWindow(id int) error {
	if w, ok := h.Wins.Get(id); ok && w.Owner == "editor" && !w.Exited() {
		if arm := h.closeArm.Load(); arm == nil || arm.id != id || time.Now().After(arm.until) {
			h.closeArm.Store(&winArm{id, time.Now().Add(3 * time.Second)})
			return fmt.Errorf("window %d is an editor and may hold unsaved changes: close it again to kill it", id)
		}
	}
	if !h.closeWindow(id) {
		return fmt.Errorf("no window %d", id)
	}
	return nil
}

func (h *Host) closeWindow(id int) bool {
	ok := h.Wins.Close(id)
	if ok {
		h.EmitAsync("win.close", map[string]any{"id": id})
		h.refresh()
	}
	return ok
}

// refuseWin raises unless user config, shipped code or the origin that
// opened the window is calling: a focused window takes every key, and
// closing one kills the user's or the model's program.
func (h *Host) refuseWin(L *glua.LState, what string, id int) {
	o := h.RT.CurrentOrigin(L)
	if w, ok := h.Wins.Get(id); ok && o != lua.OriginUser && o != lua.OriginBuiltin && w.Origin != string(o) {
		L.RaiseError("zemi.win.%s: %s code may not %s window %d", what, o, what, id)
	}
}

// hiddenWin reports whether Lua code may not see w: an editor window holds
// what the user types, so only user config sees it, never the Lua tool.
func (h *Host) hiddenWin(L *glua.LState, w *win.Window) bool {
	if w.Owner != "editor" {
		return false
	}
	tool, _ := h.modelLuaRun(L)
	return tool || h.RT.CurrentOrigin(L) != lua.OriginUser
}

func (h *Host) focusWindow(id int) {
	if ui := h.hooks(); ui != nil {
		ui.FocusWindow(id)
	}
}

func windowInfo(w *win.Window) map[string]any {
	cols, rows := w.Size()
	scr := w.Screen()
	l := w.Layout()
	m := map[string]any{
		"id": w.ID, "title": w.Title(), "kind": l.Kind, "hidden": l.Hidden, "cmd": w.Cmd, "owner": w.Owner,
		"cols": cols, "rows": rows, "running": !w.Exited(),
		"cursor": map[string]any{"x": scr.CursorX, "y": scr.CursorY, "visible": scr.HasCursor},
	}
	if w.IsText() {
		m["text"] = true
	}
	if l.Frac > 0 {
		m["frac"] = l.Frac
	}
	if l.Kind == win.KindFloat && l.X >= 0 {
		m["x"], m["y"] = l.X, l.Y
	}
	if w.Exited() {
		m["code"] = w.ExitCode()
	}
	return m
}

// layoutPatch applies the layout fields a caller may set: kind, hidden,
// frac (a split's share), cols and rows (a float's size), x and y (a
// float's top-left, -1 centers). Nil fields keep their value.
type layoutPatch struct {
	Kind   *string
	Hidden *bool
	Frac   *float64
	Cols   *int
	Rows   *int
	X, Y   *int
}

func (h *Host) applyLayout(w *win.Window, p layoutPatch) error {
	err := w.SetLayout(func(l *win.Layout) {
		if p.Kind != nil {
			l.Kind = *p.Kind
		}
		if p.Hidden != nil {
			l.Hidden = *p.Hidden
		}
		if p.Frac != nil {
			l.Frac = *p.Frac
		}
		if p.Cols != nil {
			l.Cols = *p.Cols
		}
		if p.Rows != nil {
			l.Rows = *p.Rows
		}
		if p.X != nil {
			l.X = *p.X
		}
		if p.Y != nil {
			l.Y = *p.Y
		}
	})
	if err == nil {
		h.refresh()
	}
	return err
}

// mouseEvent builds a window mouse event from plain fields: kind is click
// (press and release), press, release, move or wheel; button is left,
// middle, right, wheel_up or wheel_down. Coordinates are 0-based cells.
func mouseEvent(kind, button string, x, y int, mod string) ([]uv.MouseEvent, error) {
	b, ok := map[string]uv.MouseButton{"": uv.MouseLeft, "left": uv.MouseLeft, "middle": uv.MouseMiddle, "right": uv.MouseRight,
		"wheel_up": uv.MouseWheelUp, "wheel_down": uv.MouseWheelDown, "none": uv.MouseNone}[button]
	if !ok {
		return nil, fmt.Errorf("mouse: button %q is not left, middle, right, wheel_up or wheel_down", button)
	}
	var km uv.KeyMod
	for m := range strings.SplitSeq(mod, "+") {
		switch strings.TrimSpace(m) {
		case "":
		case "ctrl", "c":
			km |= uv.ModCtrl
		case "alt", "a", "m":
			km |= uv.ModAlt
		case "shift", "s":
			km |= uv.ModShift
		default:
			return nil, fmt.Errorf("mouse: mod %q is not ctrl, alt or shift", m)
		}
	}
	m := uv.Mouse{X: x, Y: y, Button: b, Mod: km}
	switch kind {
	case "wheel":
		if b != uv.MouseWheelUp && b != uv.MouseWheelDown {
			return nil, errors.New("mouse: wheel needs button wheel_up or wheel_down")
		}
		return []uv.MouseEvent{uv.MouseWheelEvent(m)}, nil
	case "", "click":
		if b == uv.MouseWheelUp || b == uv.MouseWheelDown {
			return []uv.MouseEvent{uv.MouseWheelEvent(m)}, nil
		}
		return []uv.MouseEvent{uv.MouseClickEvent(m), uv.MouseReleaseEvent(m)}, nil
	case "press":
		return []uv.MouseEvent{uv.MouseClickEvent(m)}, nil
	case "release":
		return []uv.MouseEvent{uv.MouseReleaseEvent(m)}, nil
	case "move", "drag":
		return []uv.MouseEvent{uv.MouseMotionEvent(m)}, nil
	}
	return nil, fmt.Errorf("mouse: kind %q is not click, press, release, move or wheel", kind)
}

// ---------------------------------------------------------------------------
// zemi.win

func (h *Host) winNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	handle := func(L *glua.LState, w *win.Window) *glua.LTable {
		t := L.NewTable()
		t.RawSetString("id", glua.LNumber(w.ID))
		// Input runs commands in the window's program (a shell reads it), so
		// it needs the same right as starting that command.
		t.RawSetString("send", L.NewFunction(func(L *glua.LState) int {
			h.luaGate(L, "lua.job.start", w.Cmd)
			return luaResult(L, w.SendText(L.CheckString(2)))
		}))
		t.RawSetString("keys", L.NewFunction(func(L *glua.LState) int {
			h.luaGate(L, "lua.job.start", w.Cmd)
			return luaResult(L, w.SendKeys(L.CheckString(2)))
		}))
		// mouse({ kind, button, x, y, mod })
		t.RawSetString("mouse", L.NewFunction(func(L *glua.LState) int {
			h.luaGate(L, "lua.job.start", w.Cmd)
			spec := L.CheckTable(2)
			evs, err := mouseEvent(rawString(spec, "kind"), rawString(spec, "button"), rawInt(spec, "x"), rawInt(spec, "y"), rawString(spec, "mod"))
			if err != nil {
				return luaResult(L, err)
			}
			for _, ev := range evs {
				w.SendMouse(ev)
			}
			return luaResult(L, nil)
		}))
		t.RawSetString("text", L.NewFunction(func(L *glua.LState) int {
			L.Push(glua.LString(w.Text()))
			return 1
		}))
		// set_text(text) and append(text) replace or grow a text window's body
		setText := func(add bool) glua.LGFunction {
			return func(L *glua.LState) int {
				h.refuseWin(L, "set_text", w.ID)
				err := w.SetText(L.CheckString(2), add)
				h.refresh()
				return luaResult(L, err)
			}
		}
		t.RawSetString("set_text", L.NewFunction(setText(false)))
		t.RawSetString("append", L.NewFunction(setText(true)))
		// wait_text(want, timeout_ms) blocks the interpreter: keep it short
		t.RawSetString("wait_text", L.NewFunction(func(L *glua.LState) int {
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(L.OptInt(3, 5000))*time.Millisecond)
			defer cancel()
			text, ok := w.WaitText(ctx, L.CheckString(2))
			L.Push(glua.LBool(ok))
			L.Push(glua.LString(text))
			return 2
		}))
		t.RawSetString("info", L.NewFunction(func(L *glua.LState) int {
			L.Push(lua.FromGo(L, windowInfo(w)))
			return 1
		}))
		t.RawSetString("focus", L.NewFunction(func(L *glua.LState) int {
			h.refuseWin(L, "focus", w.ID)
			h.focusWindow(w.ID)
			return 0
		}))
		// set({ kind, hidden, frac, cols, rows, x, y, title })
		t.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
			h.refuseWin(L, "set", w.ID)
			spec := L.CheckTable(2)
			var p layoutPatch
			if s, ok := spec.RawGetString("kind").(glua.LString); ok {
				v := string(s)
				p.Kind = &v
			}
			if b, ok := spec.RawGetString("hidden").(glua.LBool); ok {
				v := bool(b)
				p.Hidden = &v
			}
			num := func(key string) *int {
				if n, ok := spec.RawGetString(key).(glua.LNumber); ok {
					v := int(n)
					return &v
				}
				return nil
			}
			if n, ok := spec.RawGetString("frac").(glua.LNumber); ok {
				v := float64(n)
				p.Frac = &v
			}
			p.Cols, p.Rows, p.X, p.Y = num("cols"), num("rows"), num("x"), num("y")
			if s, ok := spec.RawGetString("title").(glua.LString); ok {
				w.SetTitle(sanitize.String(string(s)))
			}
			return luaResult(L, h.applyLayout(w, p))
		}))
		t.RawSetString("close", L.NewFunction(func(L *glua.LState) int {
			h.refuseWin(L, "close", w.ID)
			h.closeWindow(w.ID)
			return 0
		}))
		lua.GuardFuncs(L, t, "win:", h.guardModelLua)
		return t
	}

	// open(spec): spec = { cmd, kind, title, cols, rows, focus, on_exit(info) }
	tb.RawSetString("open", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.win.open")
		spec, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.win.open: spec must be a table")
		}
		cmd := rawString(spec, "cmd")
		if cmd == "" {
			L.RaiseError("zemi.win.open: cmd is required")
		}
		if perm.Normalize(cmd).ForceAsk != "" {
			L.RaiseError("zemi.win.open: command is refused by the environment gate")
		}
		h.luaGate(L, "lua.job.start", cmd)
		at := h.callerOf(L)
		origin := h.RT.CurrentOrigin(L)
		tool, _ := h.modelLuaRun(L)
		w, err := h.openWindow(win.Spec{
			Cmd: cmd, Kind: rawString(spec, "kind"), Title: rawString(spec, "title"),
			Cols: rawInt(spec, "cols"), Rows: rawInt(spec, "rows"), Owner: "lua",
			Origin:    string(origin),
			Clipboard: (origin == lua.OriginUser || origin == lua.OriginBuiltin) && !tool,
		})
		if err != nil {
			return luaResult(L, err)
		}
		if rawBool(spec, "hidden") {
			_ = h.applyLayout(w, layoutPatch{Hidden: new(true)})
		}
		if fn, ok := spec.RawGetString("on_exit").(*glua.LFunction); ok {
			h.GoSafe("win on_exit", func() {
				<-w.Done()
				h.callLuaAsync(at, fn, windowInfo(w))
			})
		}
		if rawBool(spec, "focus") {
			h.focusWindow(w.ID)
		}
		L.Push(handle(L, w))
		return 1
	}))

	// text(spec): spec = { text, markdown, kind, title, cols, rows, focus, hidden }
	tb.RawSetString("text", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.win.text")
		spec, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.win.text: spec must be a table")
		}
		w, err := h.openTextWindow(win.Spec{
			Text: rawString(spec, "text"), Markdown: rawBool(spec, "markdown"),
			Kind: rawString(spec, "kind"), Title: rawString(spec, "title"),
			Cols: rawInt(spec, "cols"), Rows: rawInt(spec, "rows"),
			Owner: "lua", Origin: string(h.RT.CurrentOrigin(L)),
		})
		if err != nil {
			return luaResult(L, err)
		}
		if rawBool(spec, "hidden") {
			_ = h.applyLayout(w, layoutPatch{Hidden: new(true)})
		}
		if rawBool(spec, "focus") {
			h.focusWindow(w.ID)
		}
		L.Push(handle(L, w))
		return 1
	}))

	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		if w, ok := h.Wins.Get(L.CheckInt(1)); ok && !h.hiddenWin(L, w) {
			L.Push(handle(L, w))
			return 1
		}
		L.Push(glua.LNil)
		return 1
	}))

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		var out []any
		for _, w := range h.Wins.List() {
			if !h.hiddenWin(L, w) {
				out = append(out, windowInfo(w))
			}
		}
		L.Push(lua.FromGo(L, out))
		return 1
	}))

	tb.RawSetString("close", L.NewFunction(func(L *glua.LState) int {
		id := L.CheckInt(1)
		h.refuseWin(L, "close", id)
		L.Push(glua.LBool(h.closeWindow(id)))
		return 1
	}))

	tb.RawSetString("focus", L.NewFunction(func(L *glua.LState) int {
		id := L.OptInt(1, 0)
		h.refuseWin(L, "focus", id)
		h.focusWindow(id)
		return 0
	}))

	return tb
}

// WindowCommandsStep registers /win: the keyboard's way to list, focus
// and close windows.
func (h *Host) WindowCommandsStep() lua.BootStep {
	return lua.BootStep{
		Name: "window commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			_, err := h.Commands.Add(&Command{
				Name: "win", NArgs: "*", Desc: "manage windows", Hidden: true,
				GoFn: func(h *Host, args string) error {
					fields := strings.Fields(args)
					switch {
					case len(fields) == 0 || fields[0] == "list":
						ui := h.hooks()
						if ui == nil {
							return nil
						}
						var items []any
						for _, w := range h.Wins.List() {
							state := "running"
							switch {
							case w.Exited():
								state = fmt.Sprintf("exited %d", w.ExitCode())
							case w.IsText():
								state = "text"
							}
							// a hidden window cannot take focus: its keys would go nowhere visible
							actions := []any{map[string]any{"text": "Focus", "run": fmt.Sprintf("win focus %d", w.ID)}, map[string]any{"text": "Hide", "run": fmt.Sprintf("win hide %d", w.ID)}}
							if w.Hidden() {
								state += " · hidden"
								actions = []any{map[string]any{"text": "Show", "run": fmt.Sprintf("win show %d", w.ID)}}
							}
							items = append(items, map[string]any{
								"text":    fmt.Sprintf("%d %s", w.ID, w.Title()),
								"desc":    fmt.Sprintf("%s · %s · %s", w.Kind(), w.Owner, state),
								"actions": append(actions, map[string]any{"text": "Close", "run": fmt.Sprintf("win close %d", w.ID)}),
							})
						}
						ui.OpenOverlay(Overlay{ID: "win", Title: "Windows", Origin: lua.OriginBuiltin, Items: items, Empty: "No windows open."})
						return nil
					case fields[0] == "close" && len(fields) == 2 && fields[1] == "all":
						for _, w := range h.Wins.List() {
							if w.Owner != "editor" || w.Exited() {
								h.closeWindow(w.ID)
							}
						}
						return nil
					case (fields[0] == "hide" || fields[0] == "show") && len(fields) == 2:
						id, err := strconv.Atoi(fields[1])
						w, ok := h.Wins.Get(id)
						if err != nil || !ok {
							return fmt.Errorf("no window %s", fields[1])
						}
						hidden := fields[0] == "hide"
						return h.applyLayout(w, layoutPatch{Hidden: &hidden})
					case (fields[0] == "close" || fields[0] == "focus") && len(fields) == 2:
						id, err := strconv.Atoi(fields[1])
						if err != nil {
							return fmt.Errorf("usage: /win %s <id>", fields[0])
						}
						if fields[0] == "focus" {
							if _, ok := h.Wins.Get(id); !ok {
								return fmt.Errorf("no window %d", id)
							}
							h.focusWindow(id)
							return nil
						}
						return h.CloseWindow(id)
					}
					return errors.New("usage: /win [list | focus <id> | hide <id> | show <id> | close <id|all>]")
				},
				GoComplete: subcommandCompleter([]string{"list", "focus", "hide", "show", "close"}, nil),
				Origin:     lua.OriginBuiltin, Gen: Gen(g.ID)})
			return err
		},
	}
}

// ---------------------------------------------------------------------------
// the window tool

type windowTool struct {
	h     *Host
	owner string          // "model" for the main agent, else the subagent's name
	pipe  *tools.Pipeline // the run's gate; nil = the main pipeline
}

func (h *Host) windowTool() fantasy.AgentTool { return &windowTool{h: h, owner: "model"} }

// windowToolFor is a subagent's window tool: its asks carry the agent's
// name, and its windows open hidden (its own space) until shown.
func (h *Host) windowToolFor(name string, pipe *tools.Pipeline) fantasy.AgentTool {
	return &windowTool{h: h, owner: "agent:" + name, pipe: pipe}
}

// mine reports whether the tool may act on w: the main agent on its own and
// its subagents' windows, a subagent on its own. The user's, Lua's and the
// editor's windows are not the model's to read or close.
func (t *windowTool) mine(w *win.Window) bool {
	return w.Owner == t.owner || t.owner == "model" && strings.HasPrefix(w.Owner, "agent:")
}

func (t *windowTool) gate() *tools.Pipeline {
	if t.pipe != nil {
		return t.pipe
	}
	return t.h.PipelineFor()
}

func (t *windowTool) Info() fantasy.ToolInfo {
	info := fantasy.ToolInfo{
		Name: "window",
		Description: `Run a program in a window inside zemi's screen and keep working while it runs: a TUI app you built, a dev server's log, a test watcher, a REPL. The window is a real terminal; the program sees TERM=xterm-256color and the window's size. The user can click a window and type into it (ctrl+\ brings their keys back to zemi).

Actions:
- text: open a window that shows text instead of a program: with markdown true it renders like the chat (headings, lists, tables, code, colors) and re-wraps when the window resizes. A plan, a checklist, a diff summary or a table stays on screen while you work, and set_text replaces it in place.
- set_text: replace a text window's body, or add to it with append true.
- open: run cmd; returns the window id. kind split goes below the chat and vsplit is a full-height sidebar, or wherever the user's layout puts them (a centered float when it has no place for them); float is over everything.
- set: move, resize, retitle, hide or show a window; the program keeps running. To turn a split into a sidebar, set kind vsplit.
- hide, show: a hidden window keeps running and still takes read and keys, so you can drive several programs and show the one that matters.
- focus: give the user's keys to the window.
- read: the screen as text plus the cursor. Always read after acting before deciding the next step.
- keys: press keys in neovim notation, e.g. "ihello<esc>:wq<cr>" or "<c-b>:q<cr>".
- mouse: click, press, release or move at a cell, or scroll with button wheel_up or wheel_down.
- send: raw text, as a paste without bracketing; prefer keys.
- wait: block until the program exits.
- close: end it. list: the windows you opened.
A window stays on screen after its program exits, exit code in the title, until closed; when it exits on its own, the next message tells you. A subagent's windows open hidden: show one only when the user should see it.`,
		Parameters: map[string]any{
			"action":     map[string]any{"type": "string", "enum": []string{"text", "set_text", "open", "set", "hide", "show", "focus", "read", "keys", "mouse", "send", "wait", "close", "list"}},
			"id":         map[string]any{"type": "number"},
			"cmd":        map[string]any{"type": "string", "description": "run with sh -c in the project directory"},
			"markdown":   map[string]any{"type": "boolean", "description": "text: render the body as markdown"},
			"append":     map[string]any{"type": "boolean", "description": "set_text: add to the body instead of replacing it"},
			"kind":       map[string]any{"type": "string", "enum": []string{"split", "vsplit", "float"}, "description": "default split"},
			"title":      map[string]any{"type": "string"},
			"hidden":     map[string]any{"type": "boolean"},
			"focus":      map[string]any{"type": "boolean", "description": "give the user's keys to the window right away"},
			"frac":       map[string]any{"type": "number", "description": "0.1 to 0.9: a split or vsplit's share of the screen area it sits in (by default the rows of the chat area, the columns of the screen); unset, the window takes an equal share of what is left"},
			"cols":       map[string]any{"type": "number", "description": "a float's width"},
			"rows":       map[string]any{"type": "number", "description": "a float's height"},
			"x":          map[string]any{"type": "number", "description": "mouse: 0-based column inside the window; set: a float's left column (-1 centers)"},
			"y":          map[string]any{"type": "number", "description": "mouse: 0-based row inside the window; set: a float's top row (-1 centers)"},
			"expect":     map[string]any{"type": "string", "description": "read returns as soon as the screen shows this text"},
			"wait_ms":    map[string]any{"type": "number", "description": "read waits this long for the program to draw (default 300); with expect, the longest to wait (default 5000)"},
			"scrollback": map[string]any{"type": "number", "description": "read adds this many lines that scrolled off the top"},
			"keys":       map[string]any{"type": "string"},
			"text":       map[string]any{"type": "string", "description": "send: what to type; text and set_text: the body to show"},
			"button":     map[string]any{"type": "string", "enum": []string{"left", "middle", "right", "wheel_up", "wheel_down"}, "description": "default left"},
			"mkind":      map[string]any{"type": "string", "enum": []string{"click", "press", "release", "move"}, "description": "default click"},
			"mod":        map[string]any{"type": "string", "description": "ctrl, alt or shift, joined with +"},
			"timeout_ms": map[string]any{"type": "number", "description": "wait gives up after this long (default 60000)"},
		},
		Required: []string{"action"},
	}
	if t.h.sandboxLive() && t.h.hooks() != nil {
		info.Parameters["sandbox"] = tools.SandboxProp()
	}
	return info
}

func (t *windowTool) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (t *windowTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (t *windowTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	h := t.h
	var in struct {
		Action  string   `json:"action"`
		Cmd     string   `json:"cmd"`
		Kind    string   `json:"kind"`
		Title   string   `json:"title"`
		Cols    *int     `json:"cols"`
		Rows    *int     `json:"rows"`
		Focus   bool     `json:"focus"`
		Hidden  *bool    `json:"hidden"`
		Frac    *float64 `json:"frac"`
		Scroll  int      `json:"scrollback"`
		X       *int     `json:"x"`
		Y       *int     `json:"y"`
		ID      int      `json:"id"`
		Text    string   `json:"text"`
		Keys    string   `json:"keys"`
		MD      bool     `json:"markdown"`
		Append  bool     `json:"append"`
		Expect  string   `json:"expect"`
		Button  string   `json:"button"`
		MKind   string   `json:"mkind"`
		Mod     string   `json:"mod"`
		WaitMS  float64  `json:"wait_ms"`
		Timeout float64  `json:"timeout_ms"`
		Sandbox *bool    `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(call.Input), &in); err != nil {
		return fantasy.NewTextErrorResponse("invalid tool input: " + err.Error()), nil
	}
	if resp, stop, err := h.preCheck("window", call.Input); stop {
		return resp, err
	}
	if h.hooks() == nil {
		return fantasy.NewTextErrorResponse("windows need the TUI; there is none in this run"), nil
	}
	find := func() (*win.Window, fantasy.ToolResponse, bool) {
		w, ok := h.Wins.Get(in.ID)
		if !ok || !t.mine(w) {
			return nil, fantasy.NewTextErrorResponse(fmt.Sprintf("no window %d (list shows the open ones)", in.ID)), false
		}
		return w, fantasy.ToolResponse{}, true
	}
	describe := func(w *win.Window) string {
		b, _ := json.Marshal(windowInfo(w))
		return string(b)
	}
	// opened places a new window: a subagent's own space stays hidden until
	// it shows it, and the result names the window.
	opened := func(w *win.Window) fantasy.ToolResponse {
		hidden := t.owner != "model"
		if in.Hidden != nil {
			hidden = *in.Hidden
		}
		if hidden {
			_ = h.applyLayout(w, layoutPatch{Hidden: &hidden})
		}
		if in.Focus && !hidden {
			h.focusWindow(w.ID)
		}
		h.setToolDetail(call.ID, fmt.Sprintf("window %d: %s", w.ID, w.Title()))
		return fantasy.NewTextResponse("opened " + describe(w))
	}
	switch in.Action {
	case "text", "set_text":
		// no program runs, so the gate is about what the user's screen shows
		title := cmp.Or(in.Title, "text")
		if v := t.gate().Check(tools.Request{Tool: "window", Subject: "tool.window.text", Arg: title, Reason: "show text in a window", Preview: in.Text}); v.Decision != perm.Allow {
			return fantasy.NewTextErrorResponse("permission denied (" + v.DecidedBy + "): " + v.Reason), nil
		}
		if in.Action == "set_text" {
			w, resp, ok := find()
			if !ok {
				return resp, nil
			}
			if err := w.SetText(in.Text, in.Append); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			h.refresh()
			return fantasy.NewTextResponse("shown in " + describe(w)), nil
		}
		spec := win.Spec{Text: in.Text, Markdown: in.MD, Kind: in.Kind, Title: title, Owner: t.owner}
		if in.Cols != nil {
			spec.Cols = *in.Cols
		}
		if in.Rows != nil {
			spec.Rows = *in.Rows
		}
		w, err := h.openTextWindow(spec)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		return opened(w), nil
	case "open":
		if strings.TrimSpace(in.Cmd) == "" {
			return fantasy.NewTextErrorResponse("open needs cmd"), nil
		}
		// the same gate as bash: rules, modes and the user see the command
		gate := t.gate()
		req := tools.Request{Tool: "window", Subject: "tool.bash", Arg: in.Cmd, Reason: "run in a window: " + in.Title, Unsandboxed: in.Sandbox != nil && !*in.Sandbox}
		v := gate.Check(req)
		if v.Decision != perm.Allow {
			return fantasy.NewTextErrorResponse("permission denied (" + v.DecidedBy + "): " + in.Cmd + ": " + v.Reason), nil
		}
		spec := win.Spec{Cmd: in.Cmd, Kind: in.Kind, Title: in.Title, Owner: t.owner}
		var after func() string
		if pol := gate.SandboxFor(req, v); pol != nil {
			spec.Wrap = func(c *exec.Cmd) error { return sandbox.Wrap(c, *pol) }
			after = h.guardSandbox(*pol)
		}
		if in.Cols != nil {
			spec.Cols = *in.Cols
		}
		if in.Rows != nil {
			spec.Rows = *in.Rows
		}
		w, err := h.openWindow(spec)
		if err != nil {
			if after != nil {
				after()
			}
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		if after != nil {
			h.winGuards.Store(w.ID, after)
			// the program may have exited before the guard was stored
			if w.Exited() {
				h.finishWinGuard(w)
			}
		}
		return opened(w), nil
	case "set", "hide", "show", "focus":
		w, resp, ok := find()
		if !ok {
			return resp, nil
		}
		p := layoutPatch{Frac: in.Frac, Cols: in.Cols, Rows: in.Rows, X: in.X, Y: in.Y, Hidden: in.Hidden}
		if in.Kind != "" {
			p.Kind = &in.Kind
		}
		if in.Title != "" {
			w.SetTitle(sanitize.String(in.Title))
		}
		if in.Action != "set" {
			p = layoutPatch{Hidden: new(in.Action == "hide")}
		}
		if err := h.applyLayout(w, p); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		if in.Action == "focus" || (in.Focus && !w.Hidden()) {
			h.focusWindow(w.ID)
		}
		return fantasy.NewTextResponse(describe(w)), nil
	case "read":
		w, resp, ok := find()
		if !ok {
			return resp, nil
		}
		wait := 300 * time.Millisecond
		if in.WaitMS > 0 {
			wait = time.Duration(min(in.WaitMS, 30000)) * time.Millisecond
		}
		note := ""
		var text string
		if in.Expect != "" {
			if in.WaitMS <= 0 {
				wait = 5 * time.Second
			}
			wctx, cancel := context.WithTimeout(ctx, wait)
			var ok bool
			text, ok = w.WaitText(wctx, in.Expect)
			cancel()
			if !ok {
				note = fmt.Sprintf("\n(the screen did not show %q within %s)", in.Expect, wait)
			}
		} else {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return fantasy.NewTextErrorResponse("interrupted"), nil
			}
			text = w.Text()
		}
		if text == "" {
			text = "(blank screen)"
		}
		if in.Scroll > 0 {
			if sb := w.ScrollbackText(min(in.Scroll, 2000)); sb != "" {
				text = sb + "\n----- screen -----\n" + text
			}
		}
		return fantasy.NewTextResponse(describe(w) + note + "\n<screen>\n" + sanitize.String(text) + "\n</screen>"), nil
	case "keys":
		w, resp, ok := find()
		if !ok {
			return resp, nil
		}
		if v := t.gate().Check(tools.Request{Tool: "window", Subject: "tool.window", Arg: in.Keys, Reason: "press keys in window " + w.Title(), Unsandboxed: !w.Sandboxed}); v.Decision != perm.Allow {
			return fantasy.NewTextErrorResponse("permission denied (" + v.DecidedBy + ")"), nil
		}
		if err := w.SendKeys(in.Keys); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		return fantasy.NewTextResponse("pressed; read the window to see the result"), nil
	case "mouse":
		w, resp, ok := find()
		if !ok {
			return resp, nil
		}
		mx, my := 0, 0
		if in.X != nil {
			mx = *in.X
		}
		if in.Y != nil {
			my = *in.Y
		}
		evs, err := mouseEvent(in.MKind, in.Button, mx, my, in.Mod)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		cols, rows := w.Size()
		if mx < 0 || my < 0 || mx >= cols || my >= rows {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("x, y must be inside the %dx%d window", cols, rows)), nil
		}
		// a click acts like a key press, so it asks the same gate as keys
		arg := fmt.Sprintf("%s %s %d,%d", cmp.Or(in.MKind, "click"), cmp.Or(in.Button, "left"), mx, my)
		if v := t.gate().Check(tools.Request{Tool: "window", Subject: "tool.window", Arg: arg, Reason: "use the mouse in window " + w.Title(), Unsandboxed: !w.Sandboxed}); v.Decision != perm.Allow {
			return fantasy.NewTextErrorResponse("permission denied (" + v.DecidedBy + ")"), nil
		}
		for _, ev := range evs {
			w.SendMouse(ev)
		}
		return fantasy.NewTextResponse("sent; read the window to see the result"), nil
	case "send":
		w, resp, ok := find()
		if !ok {
			return resp, nil
		}
		// typed text is a command to whatever runs there: only a sandboxed
		// program's input is a mode's to settle
		if v := t.gate().Check(tools.Request{Tool: "window", Subject: "tool.window", Arg: in.Text, Reason: "type into window " + w.Title(), Unsandboxed: !w.Sandboxed}); v.Decision != perm.Allow {
			return fantasy.NewTextErrorResponse("permission denied (" + v.DecidedBy + ")"), nil
		}
		if err := w.SendText(in.Text); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		return fantasy.NewTextResponse("sent"), nil
	case "wait":
		w, resp, ok := find()
		if !ok {
			return resp, nil
		}
		timeout := 60 * time.Second
		if in.Timeout > 0 {
			timeout = time.Duration(in.Timeout) * time.Millisecond
		}
		wctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := w.Wait(wctx); err != nil {
			return fantasy.NewTextResponse("still running after " + timeout.String() + ": " + describe(w)), nil
		}
		return fantasy.NewTextResponse("exited: " + describe(w)), nil
	case "close":
		if _, resp, ok := find(); !ok {
			return resp, nil
		}
		h.closeWindow(in.ID)
		return fantasy.NewTextResponse("closed"), nil
	case "list":
		wins := slices.DeleteFunc(h.Wins.List(), func(w *win.Window) bool { return !t.mine(w) })
		if len(wins) == 0 {
			return fantasy.NewTextResponse("no windows open"), nil
		}
		var b strings.Builder
		for _, w := range wins {
			b.WriteString(describe(w) + "\n")
		}
		return fantasy.NewTextResponse(strings.TrimRight(b.String(), "\n")), nil
	}
	return fantasy.NewTextErrorResponse("action must be text, set_text, open, set, hide, show, focus, read, keys, mouse, send, wait, close or list"), nil
}
