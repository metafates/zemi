// win.go exposes windows (internal/win) to Lua as zemi.win and to the
// model as the window tool: a program running in a split or a float over
// the chat, driven and read back without leaving zemi. Windows are host
// state: they survive turns and config reloads; only Lua callbacks are
// generation-tagged.
package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	uv "github.com/charmbracelet/ultraviolet"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
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
		spec.Env = os.Environ()
	}
	w, err := h.Wins.Open(spec)
	if err != nil {
		return nil, err
	}
	h.EmitAsync("win.open", map[string]any{"id": w.ID, "cmd": w.Cmd, "kind": w.Kind, "title": w.Title()})
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
	h.refresh()
}

// CloseWindow ends a window from the UI.
func (h *Host) CloseWindow(id int) bool { return h.closeWindow(id) }

func (h *Host) closeWindow(id int) bool {
	ok := h.Wins.Close(id)
	if ok {
		h.EmitAsync("win.close", map[string]any{"id": id})
		h.refresh()
	}
	return ok
}

func (h *Host) focusWindow(id int) {
	if ui := h.hooks(); ui != nil {
		ui.FocusWindow(id)
	}
}

func windowInfo(w *win.Window) map[string]any {
	cols, rows := w.Size()
	scr := w.Screen()
	m := map[string]any{
		"id": w.ID, "title": w.Title(), "kind": w.Kind, "cmd": w.Cmd, "owner": w.Owner,
		"cols": cols, "rows": rows, "running": !w.Exited(),
		"cursor": map[string]any{"x": scr.CursorX, "y": scr.CursorY, "visible": scr.HasCursor},
	}
	if w.Exited() {
		m["code"] = w.ExitCode()
	}
	return m
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
	for _, m := range strings.Split(mod, "+") {
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
		t.RawSetString("send", L.NewFunction(func(L *glua.LState) int {
			return luaResult(L, w.SendText(L.CheckString(2)))
		}))
		t.RawSetString("keys", L.NewFunction(func(L *glua.LState) int {
			return luaResult(L, w.SendKeys(L.CheckString(2)))
		}))
		// mouse({ kind, button, x, y, mod })
		t.RawSetString("mouse", L.NewFunction(func(L *glua.LState) int {
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
			h.focusWindow(w.ID)
			return 0
		}))
		t.RawSetString("close", L.NewFunction(func(L *glua.LState) int {
			h.closeWindow(w.ID)
			return 0
		}))
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
		gen := h.currentGen()
		w, err := h.openWindow(win.Spec{
			Cmd: cmd, Kind: rawString(spec, "kind"), Title: rawString(spec, "title"),
			Cols: rawInt(spec, "cols"), Rows: rawInt(spec, "rows"), Owner: "lua",
		})
		if err != nil {
			return luaResult(L, err)
		}
		if fn, ok := spec.RawGetString("on_exit").(*glua.LFunction); ok {
			h.GoSafe("win on_exit", func() {
				<-w.Done()
				h.callLuaAsync(gen, fn, windowInfo(w))
			})
		}
		if rawBool(spec, "focus") {
			h.focusWindow(w.ID)
		}
		L.Push(handle(L, w))
		return 1
	}))

	tb.RawSetString("get", L.NewFunction(func(L *glua.LState) int {
		if w, ok := h.Wins.Get(L.CheckInt(1)); ok {
			L.Push(handle(L, w))
			return 1
		}
		L.Push(glua.LNil)
		return 1
	}))

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		var out []any
		for _, w := range h.Wins.List() {
			out = append(out, windowInfo(w))
		}
		L.Push(lua.FromGo(L, out))
		return 1
	}))

	tb.RawSetString("close", L.NewFunction(func(L *glua.LState) int {
		L.Push(glua.LBool(h.closeWindow(L.CheckInt(1))))
		return 1
	}))

	tb.RawSetString("focus", L.NewFunction(func(L *glua.LState) int {
		h.focusWindow(L.OptInt(1, 0))
		return 0
	}))

	return tb
}

func rawInt(t *glua.LTable, key string) int {
	if n, ok := t.RawGetString(key).(glua.LNumber); ok {
		return int(n)
	}
	return 0
}

// WindowCommandsStep registers /win: the keyboard's way to list, focus
// and close windows.
func (h *Host) WindowCommandsStep() lua.BootStep {
	return lua.BootStep{
		Name: "window commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			_, err := h.Commands.Add(&Command{
				Name: "win", NArgs: "*", Desc: "windows: list, focus <id>, close <id|all>", Hidden: true,
				GoFn: func(h *Host, args string) error {
					fields := strings.Fields(args)
					switch {
					case len(fields) == 0 || fields[0] == "list":
						wins := h.Wins.List()
						if len(wins) == 0 {
							h.Notify("info", "windows", "none open")
							return nil
						}
						var lines []string
						for _, w := range wins {
							state := "running"
							if w.Exited() {
								state = fmt.Sprintf("exited %d", w.ExitCode())
							}
							lines = append(lines, fmt.Sprintf("%d %s (%s, %s)", w.ID, w.Title(), w.Kind, state))
						}
						h.Notify("info", "windows", strings.Join(lines, "\n"))
						return nil
					case fields[0] == "close" && len(fields) == 2 && fields[1] == "all":
						for _, w := range h.Wins.List() {
							h.closeWindow(w.ID)
						}
						return nil
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
						if !h.closeWindow(id) {
							return fmt.Errorf("no window %d", id)
						}
						return nil
					}
					return errors.New("usage: /win [list | focus <id> | close <id|all>]")
				},
				GoComplete: subcommandCompleter([]string{"list", "focus", "close"}, nil),
				Origin:     lua.OriginBuiltin, Gen: Gen(g.ID)})
			return err
		},
	}
}

// ---------------------------------------------------------------------------
// the window tool

type windowTool struct{ h *Host }

func (h *Host) windowTool() fantasy.AgentTool { return &windowTool{h: h} }

func (t *windowTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name: "window",
		Description: `Run a program in a window inside zemi's screen (a split below the chat, a vsplit beside it, or a float over it) and keep working while it runs: show the user a TUI app you built, a dev server's log, a test watcher, a REPL. The window is a real terminal; the program sees TERM=xterm-256color and the window's size. The user can click a window and type into it (ctrl+\ brings their keys back to zemi).

Actions:
- open (cmd, kind, title, cols, rows, focus): returns the window id.
- read (id, expect, wait_ms): the screen as text plus the cursor. With expect it returns as soon as that text shows (or after wait_ms, saying so); without, it waits wait_ms (default 300) for the program to draw. Always read after acting before deciding the next step.
- keys (id, keys): press keys in neovim notation: plain characters type themselves, <esc> <cr> <tab> <bs> <space> <up> <down> <left> <right> <home> <end> <pgup> <pgdn> <del> <f1>..<f12> name keys, <c-x> <a-x> <s-tab> <c-s-left> add modifiers, <lt> is a literal <. "ihello<esc>:wq<cr>" edits and saves in vim.
- mouse (id, x, y, button, kind, mod): a click at 0-based cell x, y (button left, middle, right; kind click, press, release, move) or a wheel step (button wheel_up or wheel_down).
- send (id, text): raw text, as a paste without bracketing; prefer keys.
- wait (id, timeout_ms): blocks until the program exits.
- close (id): ends it. list: every window.
A window stays on screen after its program exits, exit code in the title, until closed; when it exits on its own, the next message tells you.`,
		Parameters: map[string]any{
			"action":     map[string]any{"type": "string", "enum": []string{"open", "read", "keys", "mouse", "send", "wait", "close", "list"}},
			"cmd":        map[string]any{"type": "string", "description": "open: the command, run with sh -c in the project directory"},
			"kind":       map[string]any{"type": "string", "enum": []string{"split", "vsplit", "float"}, "description": "open: where the window goes (default split)"},
			"title":      map[string]any{"type": "string", "description": "open: a short title (default: the command)"},
			"cols":       map[string]any{"type": "number", "description": "open, float only: requested width"},
			"rows":       map[string]any{"type": "number", "description": "open, float only: requested height"},
			"focus":      map[string]any{"type": "boolean", "description": "open: give the user's keys to the window right away"},
			"id":         map[string]any{"type": "number", "description": "read, send, wait, close: the window"},
			"text":       map[string]any{"type": "string", "description": "send: raw text to type"},
			"keys":       map[string]any{"type": "string", "description": "keys: key presses in neovim notation, e.g. \"<c-b>:q<cr>\""},
			"expect":     map[string]any{"type": "string", "description": "read: return as soon as the screen shows this text"},
			"x":          map[string]any{"type": "number", "description": "mouse: 0-based column inside the window"},
			"y":          map[string]any{"type": "number", "description": "mouse: 0-based row inside the window"},
			"button":     map[string]any{"type": "string", "enum": []string{"left", "middle", "right", "wheel_up", "wheel_down"}, "description": "mouse: default left"},
			"mkind":      map[string]any{"type": "string", "enum": []string{"click", "press", "release", "move"}, "description": "mouse: default click (press and release)"},
			"mod":        map[string]any{"type": "string", "description": "mouse: modifiers such as ctrl, shift+alt"},
			"wait_ms":    map[string]any{"type": "number", "description": "read: wait this long before reading (default 300)"},
			"timeout_ms": map[string]any{"type": "number", "description": "wait: give up after this long (default 60000)"},
		},
		Required: []string{"action"},
	}
}

func (t *windowTool) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (t *windowTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (t *windowTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	h := t.h
	var in struct {
		Action  string  `json:"action"`
		Cmd     string  `json:"cmd"`
		Kind    string  `json:"kind"`
		Title   string  `json:"title"`
		Cols    int     `json:"cols"`
		Rows    int     `json:"rows"`
		Focus   bool    `json:"focus"`
		ID      int     `json:"id"`
		Text    string  `json:"text"`
		Keys    string  `json:"keys"`
		Expect  string  `json:"expect"`
		X       int     `json:"x"`
		Y       int     `json:"y"`
		Button  string  `json:"button"`
		MKind   string  `json:"mkind"`
		Mod     string  `json:"mod"`
		WaitMS  float64 `json:"wait_ms"`
		Timeout float64 `json:"timeout_ms"`
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
		if !ok {
			return nil, fantasy.NewTextErrorResponse(fmt.Sprintf("no window %d (list shows the open ones)", in.ID)), false
		}
		return w, fantasy.ToolResponse{}, true
	}
	describe := func(w *win.Window) string {
		b, _ := json.Marshal(windowInfo(w))
		return string(b)
	}
	switch in.Action {
	case "open":
		if strings.TrimSpace(in.Cmd) == "" {
			return fantasy.NewTextErrorResponse("open needs cmd"), nil
		}
		// the same gate as bash: rules, modes and the user see the command
		if v := h.PipelineFor().Check(tools.Request{Tool: "window", Subject: "tool.bash", Arg: in.Cmd, Reason: "run in a window: " + in.Title}); v.Decision != perm.Allow {
			return fantasy.NewTextErrorResponse("permission denied (" + v.DecidedBy + "): " + in.Cmd), nil
		}
		w, err := h.openWindow(win.Spec{Cmd: in.Cmd, Kind: in.Kind, Title: in.Title, Cols: in.Cols, Rows: in.Rows, Owner: "model"})
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		if in.Focus {
			h.focusWindow(w.ID)
		}
		h.setToolDetail(call.ID, fmt.Sprintf("window %d: %s", w.ID, w.Title()))
		return fantasy.NewTextResponse("opened " + describe(w)), nil
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
		return fantasy.NewTextResponse(describe(w) + note + "\n<screen>\n" + sanitize.String(text) + "\n</screen>"), nil
	case "keys":
		w, resp, ok := find()
		if !ok {
			return resp, nil
		}
		if v := h.PipelineFor().Check(tools.Request{Tool: "window", Subject: "tool.window", Arg: in.Keys, Reason: "press keys in window " + w.Title()}); v.Decision != perm.Allow {
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
		evs, err := mouseEvent(in.MKind, in.Button, in.X, in.Y, in.Mod)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		cols, rows := w.Size()
		if in.X < 0 || in.Y < 0 || in.X >= cols || in.Y >= rows {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("x, y must be inside the %dx%d window", cols, rows)), nil
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
		// typed text is a command to whatever runs there: the user sees it
		if v := h.PipelineFor().Check(tools.Request{Tool: "window", Subject: "tool.window", Arg: in.Text, Reason: "type into window " + w.Title()}); v.Decision != perm.Allow {
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
		if !h.closeWindow(in.ID) {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("no window %d", in.ID)), nil
		}
		return fantasy.NewTextResponse("closed"), nil
	case "list":
		wins := h.Wins.List()
		if len(wins) == 0 {
			return fantasy.NewTextResponse("no windows open"), nil
		}
		var b strings.Builder
		for _, w := range wins {
			b.WriteString(describe(w) + "\n")
		}
		return fantasy.NewTextResponse(strings.TrimRight(b.String(), "\n")), nil
	}
	return fantasy.NewTextErrorResponse("action must be open, read, keys, mouse, send, wait, close or list"), nil
}
