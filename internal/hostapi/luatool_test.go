package hostapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if err != nil || out != "hi\t2\nreturn: {\"n\":4}" {
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
	h.SetProjectTrusted(true)
	res, err := h.proposeMemory(h.AgentSessionID(), learn.OriginLearned, "project", "the build uses make", "", "")
	if err != nil || res.ID == "" {
		t.Fatalf("propose: %+v %v", res, err)
	}
	if msg, err := h.memoryTool(h.AgentSessionID(), "project", "the build uses go build", "", res.ID[:8]); err != nil || !strings.HasPrefix(msg, "saved as ") {
		t.Fatalf("replace: %q %v", msg, err)
	}
	_, m, _ := h.locateMemory(res.ID)
	if m.SupersededBy == "" {
		t.Fatalf("replaced memory still live: %+v", m)
	}
	if err := h.Settings.SetHost("memory.autonomy", "ask"); err != nil {
		t.Fatal(err)
	}
	if msg, err := h.memoryTool(h.AgentSessionID(), "project", "", "", m.SupersededBy[:8]); err != nil || !strings.Contains(msg, "queued") {
		t.Fatalf("forget without auto autonomy must wait for review: %q %v", msg, err)
	}
}

// TestLuaToolDefaultDeny: in trusted auto mode a Lua script that only reads
// runs with no ask; one that names any other call asks first, the same call
// hidden from that scan raises, and an approved run makes it.
func TestLuaToolDefaultDeny(t *testing.T) {
	h, _ := newTestHost(t)
	h.SetProjectTrusted(true)
	fakeSandbox(h, true)
	if err := h.SetPermMode("auto"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "f.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "made")
	tool := h.luaEvalTool()
	run := func(script string, allow bool) (*fakeUI, string) {
		ui := &fakeUI{askAllowed: allow, askBy: "user", screenshot: "screen"}
		h.SetUIHooks(ui)
		input, _ := json.Marshal(map[string]string{"description": "x", "script": strings.ReplaceAll(script, "MARKER", marker)})
		resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "lua", Input: string(input)})
		if err != nil {
			t.Fatal(err)
		}
		return ui, resp.Content
	}

	// the routes the trust and sandbox verification found
	for _, c := range []struct{ name, script, scan, member string }{
		{"composer shell", `zemi.composer.set("!touch MARKER") zemi.composer.submit()`, "zemi.composer.set", "composer.set"},
		{"editor command", `zemi.config.set("editor.command", "touch MARKER; true") zemi.ui.edit("f.txt", {}, function() end)`, "zemi.config.set", "config.set"},
		{"screening model", `zemi.provider.add({ id = "e", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", models = { { id = "m" } } }) zemi.config.set("models.small", "e/m")`, "zemi.provider.add", "provider.add"},
		{"allow rule", `zemi.permission.rule.add({ subject = "tool.bash", pattern = "zz *", decision = "allow", match = "zz a" })`, "zemi.permission.rule", "permission.rule"},
		{"job", `zemi.job.wait({ cmd = "touch", args = { "MARKER" } })`, "zemi.job.wait", "job.wait"},
		{"mcp", `zemi.mcp.add("x", { command = "true" })`, "zemi.mcp.add", "mcp.add"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := h.runModelLua(context.Background(), c.script, luaToolChunk); err == nil || !strings.Contains(err.Error(), "asks before zemi."+c.member) {
				t.Errorf("unapproved run: %v", err)
			}
			ui, out := run(c.script, false)
			if len(ui.asked) != 1 || !strings.Contains(ui.asked[0].Reason, c.scan) || !strings.Contains(out, "permission denied") {
				t.Errorf("pre-ask: %+v %q", ui.asked, out)
			}
			// the model's words are the description, not part of zemi's reason
			if a := ui.asked[0]; a.Description != "x" || strings.HasSuffix(a.Reason, ": x") {
				t.Errorf("ask fields: %+v", a)
			}
			if ui, out = run(c.script, true); len(ui.asked) != 1 || strings.Contains(out, "asks before") {
				t.Errorf("approved run: %q", out)
			}
		})
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("the approved job did not run")
	}

	read := `return zemi.session.id() .. zemi.ui.screenshot(), #zemi.errors.list(), zemi.config.get("editor.command") ~= nil, zemi.agent.current().id`
	if ui, out := run(read, false); len(ui.asked) != 0 || !strings.Contains(out, "screen") {
		t.Errorf("read-only script: %+v %q", ui.asked, out)
	}
	// a field read does nothing on its own, and a shipped module is read-only
	for _, src := range []string{
		`return zemi.api.version`,
		`return zemi.fs.read("f.txt") ~= nil, #zemi.fs.glob("*.txt")`,
		`return type(require("zemi.util").trim)`,
	} {
		if ui, out := run(src, false); len(ui.asked) != 0 {
			t.Errorf("%s asked: %+v %q", src, ui.asked, out)
		}
	}
	for _, hidden := range []string{`local z = zemi z.job.wait({ cmd = "true" })`, `local j = _G["zemi"].job`} {
		if ui, _ := run(hidden, false); len(ui.asked) != 1 {
			t.Errorf("%s: no ask", hidden)
		}
	}

	// a callback an approved run left behind loses the approval with the run
	os.Remove(marker)
	run(`zemi.event.on("user.zz", function() zemi.job.wait({ cmd = "touch", args = { "MARKER" } }) end)`, true)
	run(`zemi.event.emit("user.zz")`, true)
	if _, err := os.Stat(marker); err == nil {
		t.Error("an earlier run's handler started a program")
	}
	if h.PermMode() != "auto" {
		t.Error("the mode changed")
	}

	// every member is read-only or guarded, so a new one asks by default
	var members []string
	for ns, ms := range bootedNamespaces(t) {
		for _, m := range ms {
			if name := strings.TrimPrefix(ns, "zemi.") + "." + m; !luaReadOnly[name] {
				members = append(members, fmt.Sprintf("%q", name))
			}
		}
	}
	out, err := h.runModelLua(context.Background(), `local open, n = {}, 0
		for _, m in ipairs({ `+strings.Join(members, ", ")+` }) do
			local v = zemi
			for part in m:gmatch("[^.]+") do v = v[part] end
			if type(v) == "function" then
				n = n + 1
				local ok, err = pcall(v)
				if ok or not tostring(err):find("asks before zemi." .. m, 1, true) then open[#open + 1] = m end
			end
		end
		return n, table.concat(open, " ")`, luaToolChunk)
	if err != nil || !strings.HasSuffix(out, `,""]`) || strings.HasPrefix(out, "return: [0,") {
		t.Errorf("unguarded members: %s %v", out, err)
	}
}

// TestLuaToolCeiling: a script the ceiling stops is told so, not "context
// canceled", and a table error value reads as JSON.
func TestLuaToolCeiling(t *testing.T) {
	h, _ := newTestHost(t)
	h.SetProjectTrusted(true)
	fakeSandbox(h, true)
	if err := h.SetPermMode("auto"); err != nil {
		t.Fatal(err)
	}
	old := luaToolCeiling
	luaToolCeiling = 50 * time.Millisecond
	t.Cleanup(func() { luaToolCeiling = old })
	tool := h.luaEvalTool()
	for _, c := range []struct{ script, want string }{
		{"while true do end", "error: stopped after 50ms: the script must finish quickly"},
		{"error({ code = 1 })", `error: lua-tool: {"code":1}`},
	} {
		input, _ := json.Marshal(map[string]string{"description": "x", "script": c.script})
		resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "lua", Input: string(input)})
		if err != nil || !resp.IsError || !strings.Contains(resp.Content, c.want) || strings.Contains(resp.Content, "context canceled") {
			t.Errorf("%s: %+v (%v)", c.script, resp, err)
		}
	}
}

// TestLuaToolPath: a script file is read the way the read tool reads it, so
// a Lua error cannot echo a secret or a file outside the jail unasked.
func TestLuaToolPath(t *testing.T) {
	h, _ := newTestHost(t)
	h.SetProjectTrusted(true)
	fakeSandbox(h, true)
	if err := h.SetPermMode("auto"); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "x.lua")
	for _, f := range []string{filepath.Join(h.Dirs.ProjectDir, "ok.lua"), filepath.Join(h.Dirs.ProjectDir, ".env.lua"), outside} {
		if err := os.WriteFile(f, []byte("return 1"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		path, want string
		asks       int
	}{
		{"ok.lua", "return: 1", 0},
		{"nope.lua", "nope.lua: no such file or directory", 0},
		{"notes.txt", "notes.txt: not a .lua file", 0},
		{".env.lua", "permission denied", 1},
		{outside, "permission denied", 1},
	} {
		ui := &fakeUI{askBy: "user"}
		h.SetUIHooks(ui)
		input, _ := json.Marshal(map[string]string{"description": "x", "path": c.path})
		resp, _ := h.luaEvalTool().Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "lua", Input: string(input)})
		if !strings.HasPrefix(resp.Content, c.want) || len(ui.asked) != c.asks {
			t.Errorf("%s: %q, %d asks", c.path, resp.Content, len(ui.asked))
		}
	}
}
