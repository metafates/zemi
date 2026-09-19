package hostapi

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/win"
)

// TestWinInputGated: typing into a window another party opened needs the
// job right for its command, like opening it would.
func TestWinInputGated(t *testing.T) {
	h, r := newTestHost(t)
	h.SetUIHooks(&fakeUI{})
	w, err := h.openWindow(win.Spec{Cmd: "sleep 5", Owner: "model"})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Wins.Close(w.ID)
	runUser(t, r, `
		local w = zemi.win.get(1)
		assert(not pcall(w.send, w, "echo hi\r"), "send")
		assert(not pcall(w.keys, w, "<c-c>"), "keys")
		assert(not pcall(w.mouse, w, { x = 0, y = 0 }), "mouse")
	`)
}

// TestWindowTool drives open, read, send, wait and close through the
// model's tool: the open goes through the bash gate, typing and clicks through
// tool.window, and an exit reaches the next message as a note.
func TestWindowTool(t *testing.T) {
	h, _ := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	tool := h.windowTool()
	run := func(input string) fantasy.ToolResponse {
		t.Helper()
		resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "w", Name: "window", Input: input})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return resp
	}
	if resp := run(`{"action":"open","cmd":"read x; echo got:$x; exit 4"}`); !resp.IsError || !strings.Contains(resp.Content, "permission denied") {
		t.Fatalf("open must ask the bash gate: %+v", resp)
	}
	h.Perm.Add(perm.Rule{Subject: "tool.bash", Pattern: "*", Decision: perm.Allow})
	resp := run(`{"action":"open","cmd":"read x; echo got:$x; exit 4","kind":"float","title":"t","focus":true}`)
	if resp.IsError || !strings.Contains(resp.Content, `"id":1`) {
		t.Fatalf("open: %+v", resp)
	}
	if resp := run(`{"action":"send","id":1,"text":"hi\r"}`); !resp.IsError {
		t.Fatalf("send must ask tool.window: %+v", resp)
	}
	if resp := run(`{"action":"mouse","id":1,"x":1,"y":0}`); !resp.IsError {
		t.Fatalf("mouse must ask tool.window: %+v", resp)
	}
	h.Perm.Add(perm.Rule{Subject: "tool.window", Pattern: "*", Decision: perm.Allow})
	if resp := run(`{"action":"keys","id":1,"keys":"h<nope>"}`); !resp.IsError || !strings.Contains(resp.Content, "unknown key") {
		t.Fatalf("bad key name must be refused: %+v", resp)
	}
	if resp := run(`{"action":"mouse","id":1,"x":500,"y":0}`); !resp.IsError {
		t.Fatalf("mouse outside the window must be refused: %+v", resp)
	}
	if resp := run(`{"action":"mouse","id":1,"x":1,"y":0}`); resp.IsError {
		t.Fatalf("mouse: %+v", resp)
	}
	if resp := run(`{"action":"keys","id":1,"keys":"hi<cr>"}`); resp.IsError {
		t.Fatalf("keys: %+v", resp)
	}
	if resp := run(`{"action":"read","id":1,"expect":"got:hi"}`); !strings.Contains(resp.Content, "got:hi") || strings.Contains(resp.Content, "did not show") {
		t.Fatalf("read with expect: %+v", resp)
	}
	if resp := run(`{"action":"read","id":1,"expect":"never","wait_ms":50}`); !strings.Contains(resp.Content, "did not show") {
		t.Fatalf("read with a missing expect must say so: %+v", resp)
	}
	if resp := run(`{"action":"wait","id":1,"timeout_ms":3000}`); !strings.Contains(resp.Content, "exited") || !strings.Contains(resp.Content, `"code":4`) {
		t.Fatalf("wait: %+v", resp)
	}
	if resp := run(`{"action":"read","id":1,"wait_ms":10}`); !strings.Contains(resp.Content, "got:hi") {
		t.Fatalf("read: %+v", resp)
	}
	if notes := h.takeShellNotes(); !strings.Contains(notes, `<window id=1 title="t" exited code=4/>`) {
		t.Fatalf("exit note: %q", notes)
	}
	if resp := run(`{"action":"list"}`); !strings.Contains(resp.Content, `"running":false`) {
		t.Fatalf("list: %+v", resp)
	}
	if resp := run(`{"action":"set","id":1,"kind":"vsplit","frac":0.4,"title":"side"}`); resp.IsError || !strings.Contains(resp.Content, `"kind":"vsplit"`) || !strings.Contains(resp.Content, `"frac":0.4`) || !strings.Contains(resp.Content, `"title":"side"`) {
		t.Fatalf("set: %+v", resp)
	}
	if resp := run(`{"action":"set","id":1,"kind":"tab"}`); !resp.IsError {
		t.Fatalf("bad kind must be refused: %+v", resp)
	}
	if resp := run(`{"action":"hide","id":1}`); !strings.Contains(resp.Content, `"hidden":true`) {
		t.Fatalf("hide: %+v", resp)
	}
	if resp := run(`{"action":"show","id":1}`); !strings.Contains(resp.Content, `"hidden":false`) {
		t.Fatalf("show: %+v", resp)
	}
	sub := h.windowToolFor("explore", nil)
	resp, _ = sub.Run(context.Background(), fantasy.ToolCall{ID: "s", Name: "window", Input: `{"action":"open","cmd":"sleep 5"}`})
	if resp.IsError || !strings.Contains(resp.Content, `"hidden":true`) || !strings.Contains(resp.Content, `"owner":"agent:explore"`) {
		t.Fatalf("subagent windows open hidden: %+v", resp)
	}
	h.Wins.Close(2)
	if resp := run(`{"action":"close","id":1}`); resp.IsError {
		t.Fatalf("close: %+v", resp)
	}
	if resp := run(`{"action":"read","id":1}`); !resp.IsError {
		t.Fatalf("closed window must be gone: %+v", resp)
	}
}

// TestWindowInputSettling: auto mode settles input only for a window that
// runs sandboxed; typing into an unconfined one asks the user.
func TestWindowInputSettling(t *testing.T) {
	h, _ := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	h.SetProjectTrusted(true)
	fakeSandbox(h, true)
	if err := h.SetPermMode("auto"); err != nil {
		t.Fatal(err)
	}
	reflectStub(t, h, "SAFE")
	for _, c := range []struct {
		name  string
		wrap  func(*exec.Cmd) error
		asked bool
	}{
		{"unconfined", nil, true},
		{"sandboxed", func(*exec.Cmd) error { return nil }, false},
	} {
		w, err := h.openWindow(win.Spec{Cmd: "cat >/dev/null", Owner: "model", Wrap: c.wrap})
		if err != nil {
			t.Fatal(err)
		}
		ui.asked = nil
		for _, in := range []string{`"action":"send","text":"x"}`, `"action":"keys","keys":"x"}`, `"action":"mouse","x":0,"y":0}`} {
			in = fmt.Sprintf(`{"id":%d,`, w.ID) + in
			resp, err := h.windowTool().Run(context.Background(), fantasy.ToolCall{ID: "w", Name: "window", Input: in})
			if err != nil || resp.IsError != c.asked {
				t.Errorf("%s %s: %+v %v", c.name, in, resp, err)
			}
		}
		if asked := len(ui.asked) == 3; asked != c.asked || (asked && !strings.Contains(ui.asked[0].Reason, "not sandboxed")) {
			t.Errorf("%s: asked %+v", c.name, ui.asked)
		}
		h.Wins.Close(w.ID)
	}
}

// TestWindowFocus: focus hands the user's keyboard over, so it asks and
// never unhides a window on its own.
func TestWindowFocus(t *testing.T) {
	h, _ := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	w, err := h.openWindow(win.Spec{Cmd: "cat >/dev/null", Owner: "model"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Wins.Close(w.ID) })
	run := func(in string) fantasy.ToolResponse {
		resp, err := h.windowTool().Run(t.Context(), fantasy.ToolCall{ID: "w", Name: "window", Input: fmt.Sprintf(in, w.ID)})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := run(`{"action":"hide","id":%d}`); resp.IsError {
		t.Fatalf("hide: %+v", resp)
	}
	if resp := run(`{"action":"focus","id":%d}`); !resp.IsError || len(ui.asked) > 0 || !w.Hidden() {
		t.Fatalf("focus unhid the window: %+v %+v", resp, ui.asked)
	}
	if resp := run(`{"action":"show","id":%d}`); resp.IsError {
		t.Fatalf("show: %+v", resp)
	}
	if resp := run(`{"action":"focus","id":%d}`); !resp.IsError {
		t.Fatalf("a denied focus must refuse: %+v", resp)
	}
	if len(ui.asked) != 1 || !strings.Contains(ui.asked[0].Reason, "keyboard") {
		t.Fatalf("focus asked %+v", ui.asked)
	}
	ui.askAllowed = true
	if resp := run(`{"action":"focus","id":%d}`); resp.IsError {
		t.Fatalf("focus: %+v", resp)
	}
}

// TestWindowOwnership: the window tool acts only on the model's windows and
// its subagents'; an editor window is hidden from Lua below user config and
// from the Lua tool, and the user must close a running one twice.
func TestWindowOwnership(t *testing.T) {
	t.Parallel()
	h, r := newTestHost(t)
	h.SetUIHooks(&fakeUI{})
	h.Perm.Add(perm.Rule{Subject: "tool.window", Pattern: "*", Decision: perm.Allow})
	open := func(owner string) int {
		w, err := h.openWindow(win.Spec{Cmd: "sleep 5", Owner: owner})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { h.Wins.Close(w.ID) })
		return w.ID
	}
	editor, user, model, sub := open("editor"), open("user"), open("model"), open("agent:explore")
	for _, c := range []struct {
		tool    fantasy.AgentTool
		id      int
		allowed bool
	}{
		{h.windowTool(), editor, false},
		{h.windowTool(), user, false},
		{h.windowTool(), sub, true},
		{h.windowToolFor("explore", nil), model, false},
		{h.windowToolFor("explore", nil), sub, true},
	} {
		for _, action := range []string{`"read","wait_ms":1`, `"keys","keys":"x"`, `"mouse"`, `"send","text":"x"`, `"hide"`, `"wait","timeout_ms":1`} {
			resp, _ := c.tool.Run(t.Context(), fantasy.ToolCall{Input: fmt.Sprintf(`{"action":%s,"id":%d}`, action, c.id)})
			if hidden := strings.Contains(resp.Content, "no window"); hidden == c.allowed {
				t.Errorf("window %d %s: %+v", c.id, action, resp)
			}
		}
	}
	if resp, _ := h.windowTool().Run(t.Context(), fantasy.ToolCall{Input: fmt.Sprintf(`{"action":"close","id":%d}`, editor)}); !resp.IsError {
		t.Fatalf("the model closed the editor: %+v", resp)
	}
	if resp, _ := h.windowTool().Run(t.Context(), fantasy.ToolCall{Input: `{"action":"list"}`}); strings.Contains(resp.Content, `"owner":"editor"`) || strings.Contains(resp.Content, `"owner":"user"`) {
		t.Fatalf("list shows others' windows: %+v", resp)
	}
	see := fmt.Sprintf(`assert(zemi.win.get(%d) == nil and #zemi.win.list() == 3, "editor visible")`, editor)
	if err := r.ExecuteChunk(t.Context(), see, "p.lua", lua.OriginProject); err != nil {
		t.Error(err)
	}
	if _, err := h.runModelLua(t.Context(), see, luaToolChunk); err != nil {
		t.Error(err)
	}
	runUser(t, r, fmt.Sprintf(`assert(zemi.win.get(%d) and #zemi.win.list() == 4, "user config sees the editor")`, editor))

	// the user's close kills a running editor only when repeated
	if err := h.CloseWindow(editor); err == nil || !strings.Contains(err.Error(), "unsaved") {
		t.Fatalf("first close of the editor: %v", err)
	}
	if err := h.CloseWindow(editor); err != nil {
		t.Fatalf("second close of the editor: %v", err)
	}
}

// TestWinNamespace: zemi.win from user Lua, gated as a job, with secret-shaped
// env names kept from the program like any child.
func TestWinNamespace(t *testing.T) {
	t.Setenv("ZEMI_TEST_API_KEY", "leaked")
	h, r := newTestHost(t)
	h.SetUIHooks(&fakeUI{})
	h.Perm.Add(perm.Rule{Subject: "lua.job.start", Pattern: "*", Decision: perm.Allow})
	runUser(t, r, `
		local w, err = zemi.win.open({ cmd = "echo lua-window ${ZEMI_TEST_API_KEY:-clean}; sleep 5", kind = "vsplit", title = "x" })
		assert(w, err)
		assert(w.id == 1, "id")
		assert(#zemi.win.list() == 1, "list")
		assert(zemi.win.get(1):info().kind == "vsplit", "kind")
		local shown = zemi.win.get(1):wait_text("lua-window clean", 3000)
		assert(shown, "wait_text")
		assert(select(2, zemi.win.get(1):keys("<bogus>")) ~= nil, "bad keys")
		assert(zemi.win.get(1):mouse({ x = 0, y = 0, button = "wheel_down", kind = "wheel" }) == true, "mouse")
		assert(zemi.win.get(1):set({ kind = "float", cols = 40, rows = 5, x = 2, y = 3 }) == true, "set")
		local info = zemi.win.get(1):info()
		assert(info.kind == "float" and info.x == 2 and info.y == 3, "layout readback")
		assert(zemi.win.get(1):set({ hidden = true }) == true and zemi.win.get(1):info().hidden, "hide")
		assert(zemi.win.get(1):keys("<c-c>") == true, "keys")
		assert(zemi.win.close(1) == true and #zemi.win.list() == 0, "close")
	`)
	if n := len(h.Wins.List()); n != 0 {
		t.Fatalf("windows left: %d", n)
	}
}

// TestWinCommandOpensMenu: /win lists windows with actions that run the
// /win subcommands.
func TestWinCommandOpensMenu(t *testing.T) {
	h, r := newTestHost(t)
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep(), h.BuiltinCommandsStep()}); err != nil {
		t.Fatal(err)
	}
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	w, err := h.openWindow(win.Spec{Cmd: "sleep 5", Owner: "user", Title: "sleeper"})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Wins.Close(w.ID)
	if err := h.Commands.Lookup("win").GoFn(h, ""); err != nil {
		t.Fatal(err)
	}
	if len(ui.opened) != 1 || len(ui.opened[0].Items) != 1 || !strings.Contains(fmt.Sprint(ui.opened[0].Items[0]), "win close 1") {
		t.Fatalf("opened: %+v", ui.opened)
	}
}

// TestWindowTextTool: a text window asks tool.window.text, which every mode
// settles; set_text replaces the body in place and only a text window takes
// it.
func TestWindowTextTool(t *testing.T) {
	h, r := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	tool := h.windowTool()
	run := func(input string) fantasy.ToolResponse {
		t.Helper()
		resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "w", Name: "window", Input: input})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return resp
	}
	resp := run(`{"action":"text","text":"# Plan","markdown":true,"title":"plan"}`)
	if resp.IsError || !strings.Contains(resp.Content, `"text":true`) || !strings.Contains(resp.Content, `"title":"plan"`) || len(ui.asked) != 0 {
		t.Fatalf("a text window settles without asking: %+v asked %+v", resp, ui.asked)
	}
	w, _ := h.Wins.Get(1)
	if resp := run(`{"action":"set_text","id":1,"text":"\n- done"}`); resp.IsError {
		t.Fatalf("set_text: %+v", resp)
	}
	if resp := run(`{"action":"set_text","id":1,"text":"# Next","append":true}`); resp.IsError || w.Text() != "\n- done# Next" {
		t.Fatalf("append: %+v body %q", resp, w.Text())
	}
	h.Perm.Add(perm.Rule{Subject: "tool.bash", Pattern: "*", Decision: perm.Allow})
	if resp := run(`{"action":"open","cmd":"sleep 5"}`); resp.IsError {
		t.Fatalf("open: %+v", resp)
	}
	defer h.Wins.CloseAll()
	if resp := run(`{"action":"set_text","id":2,"text":"x"}`); !resp.IsError {
		t.Fatalf("a program window must refuse set_text: %+v", resp)
	}
	// Lua opens one too, and only its own origin may write to it
	runUser(t, r, `
		local w = assert(zemi.win.panel({ text = "hi", markdown = true, title = "lua" }))
		assert(w:set_text("bye"))
		assert(w:append(" now"))
		assert(w:text() == "bye now", w:text())
		assert(w:info().text == true, "info.text")
		-- a name is one window: opening it again replaces it, get finds it
		local a = assert(zemi.win.panel({ text = "one", name = "notes" }))
		local b = assert(zemi.win.panel({ text = "two", name = "notes" }))
		assert(zemi.win.get(a.id) == nil and zemi.win.get("notes").id == b.id, "the second notes must replace the first")
		assert(not zemi.win.panel({ text = "three", name = "notes", kind = "bad" }) and zemi.win.get("notes").id == b.id, "a failed open must keep the old window")
		assert(zemi.win.get("none") == nil)
	`)
	if err := r.ExecuteChunk(t.Context(), `assert(zemi.win.get("notes") == nil, "a name is per origin")`, "p.lua", lua.OriginProject); err != nil {
		t.Fatal(err)
	}
	if err := r.ExecuteChunk(t.Context(), `zemi.win.get(3):set_text("x")`, "p.lua", lua.OriginProject); err == nil || !strings.Contains(err.Error(), "may not set_text") {
		t.Fatalf("project code wrote to another origin's window: %v", err)
	}
}
