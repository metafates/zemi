package hostapi

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"

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
