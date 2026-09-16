// win.go exposes windows (internal/win) to Lua as zemi.win and to the
// model as the window tool: a program running in a split or a float over
// the chat, driven and read back without leaving zemi. Windows are host
// state: they survive turns and config reloads; only Lua callbacks are
// generation-tagged.
package hostapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

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
	h.EmitAsync("win.open", map[string]any{"id": w.ID, "cmd": w.Cmd, "kind": w.Kind, "title": w.Title})
	h.refresh()
	return w, nil
}

// windowExited runs on the window's wait goroutine (Manager.OnExit).
func (h *Host) windowExited(w *win.Window) {
	h.EmitAsync("win.exit", map[string]any{"id": w.ID, "cmd": w.Cmd, "code": w.ExitCode(), "title": w.Title})
	if w.Owner == "model" {
		ag := h.agents
		ag.mu.Lock()
		ag.shellNotes = append(ag.shellNotes, fmt.Sprintf("\n<window id=%d title=%q exited code=%d/>", w.ID, w.Title, w.ExitCode()))
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
	m := map[string]any{
		"id": w.ID, "title": w.Title, "kind": w.Kind, "cmd": w.Cmd, "owner": w.Owner,
		"cols": cols, "rows": rows, "running": !w.Exited(),
	}
	if w.Exited() {
		m["code"] = w.ExitCode()
	}
	return m
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
		t.RawSetString("text", L.NewFunction(func(L *glua.LState) int {
			L.Push(glua.LString(w.Text()))
			return 1
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

// ---------------------------------------------------------------------------
// the window tool

type windowTool struct{ h *Host }

func (h *Host) windowTool() fantasy.AgentTool { return &windowTool{h: h} }

func (t *windowTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name: "window",
		Description: `Run a program in a window inside zemi's screen (a split below the chat, a vsplit beside it, or a float over it) and keep working while it runs: show the user a TUI app you built, a dev server's log, a test watcher, a REPL. The window is a real terminal; the program sees TERM=xterm-256color and the window's size. The user can click a window and type into it (ctrl+\ brings their keys back to zemi).

Actions: open (cmd, kind, title, cols, rows, focus) returns the window id; read (id, wait_ms) returns the screen as text, after waiting wait_ms for it to draw; send (id, text) types text into it, control characters included ("q", "\r", "" for ctrl+c); wait (id, timeout_ms) blocks until the program exits; close (id) ends it; list shows every window. A window stays on screen after its program exits, exit code in the title, until closed. When the program exits on its own, the next message tells you.`,
		Parameters: map[string]any{
			"action":     map[string]any{"type": "string", "enum": []string{"open", "read", "send", "wait", "close", "list"}},
			"cmd":        map[string]any{"type": "string", "description": "open: the command, run with sh -c in the project directory"},
			"kind":       map[string]any{"type": "string", "enum": []string{"split", "vsplit", "float"}, "description": "open: where the window goes (default split)"},
			"title":      map[string]any{"type": "string", "description": "open: a short title (default: the command)"},
			"cols":       map[string]any{"type": "number", "description": "open, float only: requested width"},
			"rows":       map[string]any{"type": "number", "description": "open, float only: requested height"},
			"focus":      map[string]any{"type": "boolean", "description": "open: give the user's keys to the window right away"},
			"id":         map[string]any{"type": "number", "description": "read, send, wait, close: the window"},
			"text":       map[string]any{"type": "string", "description": "send: what to type"},
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
		h.setToolDetail(call.ID, fmt.Sprintf("window %d: %s", w.ID, w.Title))
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
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return fantasy.NewTextErrorResponse("interrupted"), nil
		}
		text := w.Text()
		if text == "" {
			text = "(blank screen)"
		}
		return fantasy.NewTextResponse(describe(w) + "\n<screen>\n" + sanitize.String(text) + "\n</screen>"), nil
	case "send":
		w, resp, ok := find()
		if !ok {
			return resp, nil
		}
		// typed text is a command to whatever runs there: the user sees it
		if v := h.PipelineFor().Check(tools.Request{Tool: "window", Subject: "tool.window", Arg: in.Text, Reason: "type into window " + w.Title}); v.Decision != perm.Allow {
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
	return fantasy.NewTextErrorResponse("action must be open, read, send, wait, close or list"), nil
}
