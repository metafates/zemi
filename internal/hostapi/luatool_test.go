package hostapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/perm"
)

// TestAgentSelfAccess covers what the agent reaches of zemi itself: the lua
// tool returns prints and values without leaking globals, the scratchpad
// takes writes without asking, and memory edits apply only when autonomy
// lets them.
func TestAgentSelfAccess(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.agents.sessionID = "s1"
	ctx := context.Background()

	out, err := h.runModelLua(ctx, `print("hi", 1 + 1) leak = 1 return { n = #zemi.agent.list() }`, luaToolChunk)
	if err != nil || out != "hi\t2\nreturn: {\"n\":3}" {
		t.Fatalf("lua: %q %v", out, err)
	}
	if out, _ := h.runModelLua(ctx, `return leak == nil`, luaToolChunk); out != "return: true" {
		t.Fatalf("globals leaked between runs: %q", out)
	}

	pad := h.ScratchDir()
	if !strings.Contains(pad, filepath.Join("scratch", "s1")) {
		t.Fatalf("pad = %q", pad)
	}
	if h.fsDefault("fs.write", filepath.Join(pad, "notes.md")) != perm.Allow || h.fsDefault("fs.write", filepath.Join(h.Dirs.ProjectDir, "x")) != perm.Ask {
		t.Fatal("writes allow in the scratchpad only")
	}

	if err := h.Settings.SetHost("memory.autonomy", "auto"); err != nil {
		t.Fatal(err)
	}
	stored, id, err := h.proposeMemory("project", "fact", "the build uses make", nil)
	if err != nil || !stored {
		t.Fatalf("propose: %v %v", stored, err)
	}
	if msg, err := h.modelEditMemory(id[:8], "the build uses go build"); err != nil || !strings.HasSuffix(msg, "updated") {
		t.Fatalf("update: %q %v", msg, err)
	}
	if _, m, _ := h.locateMemory(id); m.Text != "the build uses go build" || m.Origin != learn.OriginLearned {
		t.Fatalf("text = %q origin = %q", m.Text, m.Origin)
	}
	if err := h.Settings.SetHost("memory.autonomy", "ask"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.modelEditMemory(id, ""); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("forget without auto autonomy must ask (and fail closed headless): %v", err)
	}
}

// TestLuaToolWidening: in trusted auto mode the Lua tool runs without
// asking, but a script that widens permissions or starts programs asks the
// user first (headless it is denied), and such a call it hides from that
// check, or makes from code an earlier run left behind, raises instead.
func TestLuaToolWidening(t *testing.T) {
	h, _ := newTestHost(t)
	h.SetProjectTrusted(true)
	fakeSandbox(h, true)
	if err := h.SetPermMode("auto"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "made")
	tool := h.luaEvalTool()
	for _, c := range []struct {
		name, script    string
		headless, allow bool
		asked, made     bool
		out             string
	}{
		{"inspecting asks nothing", `return #zemi.permission.mode.list() > 0`, false, false, false, false, "return: true"},
		{"allow rule denied", `zemi.permission.rule({ subject = "tool.bash", pattern = "zz *", decision = "allow", match = "zz a" }) print("added")`, false, false, true, false, "permission denied"},
		{"allow rule approved", `zemi.permission.rule({ subject = "tool.bash", pattern = "zz *", decision = "allow", match = "zz a" }) print("added")`, false, true, true, false, "added"},
		{"hidden deny rule runs", `local p = zemi["permis" .. "sion"] p.rule({ subject = "tool.bash", pattern = "yy *", decision = "deny", match = "yy a" }) print("added")`, false, false, false, false, "added"},
		{"mode set denied", `zemi.config.set("permissions.mode", "yolo")`, false, false, true, false, "permission denied"},
		{"mcp headless", `zemi.mcp.add("x", { command = "true" })`, true, true, false, false, "permission denied"},
		{"job approved", `zemi.job.out({ cmd = "touch", args = { "MARKER" } })`, false, true, true, true, ""},
		{"hidden job raises", `local j = zemi["jo" .. "b"] j.out({ cmd = "touch", args = { "MARKER" } })`, false, false, false, false, "asks the user"},
		{"hidden rule raises", `local p = zemi["permis" .. "sion"] p.rule({ subject = "tool.bash", pattern = "xx *", decision = "allow", match = "xx a" })`, false, false, false, false, "asks the user"},
		{"earlier handler", `zemi.event.on("user.zz", function() local j = zemi["jo" .. "b"] j.out({ cmd = "touch", args = { "MARKER" } }) end)`, false, false, false, false, "(no output)"},
		{"approved run calls it", `zemi.event.emit("user.zz") -- zemi.job.start`, false, true, true, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			os.Remove(marker)
			ui := &fakeUI{askAllowed: c.allow, askBy: "user"}
			h.SetUIHooks(ui)
			if c.headless {
				h.SetUIHooks(nil)
			}
			input, _ := json.Marshal(map[string]string{"description": "x", "script": strings.ReplaceAll(c.script, "MARKER", marker)})
			resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "Lua", Input: string(input)})
			if err != nil {
				t.Fatal(err)
			}
			if asked := len(ui.asked) > 0; asked != c.asked {
				t.Errorf("asked %v, want %v: %s", asked, c.asked, resp.Content)
			} else if asked && !strings.Contains(ui.asked[0].Reason, "outside the sandbox") {
				t.Errorf("ask reason %q", ui.asked[0].Reason)
			}
			if _, err := os.Stat(marker); (err == nil) != c.made {
				t.Errorf("made %v, want %v: %s", err == nil, c.made, resp.Content)
			}
			if !strings.Contains(resp.Content, c.out) {
				t.Errorf("response %q, want %q", resp.Content, c.out)
			}
		})
	}
	if h.PermMode() == "yolo" {
		t.Error("the mode changed without the user")
	}
}
