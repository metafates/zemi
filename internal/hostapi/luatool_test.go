package hostapi

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// TestAgentSelfAccess covers what the agent reaches of zemi itself: the lua
// tool returns prints and values without leaking globals, the scratchpad
// takes writes without asking, and memory edits apply only when autonomy
// lets them.
func TestAgentSelfAccess(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Dirs.ProjectDir = outsideTemp(t)
	h.agents.sessionID = "s1"
	ctx := t.Context()

	out, err := h.runModelLua(ctx, `print("hi", 1 + 1) leak = 1 return { n = #zemi.agent.list() }`, luaToolChunk)
	if err != nil || out != "hi\t2\nreturn: {\"n\":5}" {
		t.Fatalf("lua: %q %v", out, err)
	}
	if out, _ := h.runModelLua(ctx, `return leak == nil`, luaToolChunk); out != "return: true" {
		t.Fatalf("globals leaked between runs: %q", out)
	}

	pad := h.ScratchDir()
	if !strings.Contains(pad, filepath.Join("scratch", "s1")) {
		t.Fatalf("pad = %q", pad)
	}
	pipe := h.PipelineFor()
	pipe.Ask = nil
	if pipe.Check(tools.Request{Subject: perm.SubjectWrite, Arg: filepath.Join(pad, "notes.md")}).Decision != perm.Allow || pipe.Check(tools.Request{Subject: perm.SubjectWrite, Arg: filepath.Join(h.Dirs.ProjectDir, "x")}).Decision != perm.Ask {
		t.Fatal("writes allow in the scratchpad only")
	}

	if _, err := h.Settings.SetQuiet("memory.autonomy", "auto"); err != nil {
		t.Fatal(err)
	}
	h.SetProjectTrusted(true)
	res, err := h.proposeMemory(h.AgentSessionID(), store.OriginLearned, "project", "the build uses make", "", "")
	if err != nil || res.ID == "" {
		t.Fatalf("propose: %+v %v", res, err)
	}
	if msg, err := h.memoryTool(h.PipelineFor(), h.AgentSessionID(), "project", "the build uses go build", "", res.ID); err != nil || !strings.HasPrefix(msg, "saved as ") {
		t.Fatalf("replace: %q %v", msg, err)
	}
	_, m, _ := h.locateMemory(res.ID)
	if m.SupersededBy == "" {
		t.Fatalf("replaced memory still live: %+v", m)
	}
	if _, err := h.Settings.SetQuiet("memory.autonomy", "ask"); err != nil {
		t.Fatal(err)
	}
	if msg, err := h.memoryTool(h.PipelineFor(), h.AgentSessionID(), "project", "", "", m.SupersededBy); err != nil || !strings.Contains(msg, "queued") {
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
	tool := h.luaEvalTool(h.PipelineFor())
	run := func(script string, allow bool) (*fakeUI, string) {
		ui := &fakeUI{askAllowed: allow, askBy: "user", screenshot: "screen"}
		h.SetUIHooks(ui)
		input, _ := json.Marshal(map[string]string{"about": "x", "script": strings.ReplaceAll(script, "MARKER", marker)}, json.Deterministic(true))
		resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "1", Name: "lua", Input: string(input)})
		if err != nil {
			t.Fatal(err)
		}
		return ui, resp.Content
	}

	// the routes the trust and sandbox verification found
	for _, c := range []struct{ name, script, scan, member string }{
		{"composer shell", `zemi.composer.set("!touch MARKER") zemi.composer.submit()`, "zemi.composer.set", "composer.set"},
		{"editor command", `zemi.config.set("editor.command", "touch MARKER; true") zemi.ui.edit("f.txt", {}, function() end)`, "zemi.config.set", "config.set"},
		{"screening model", `zemi.provider.add({ id = "e", kind = "openai_compat", base_url = "http://127.0.0.1:1/v1", models = { { id = "m" } } }) zemi.config.set("models.titles", "e/m")`, "zemi.provider.add", "provider.add"},
		{"allow rule", `zemi.permission.rule.add({ subject = "tool.shell", pattern = "zz *", decision = "allow", match = "zz a" })`, "zemi.permission.rule", "permission.rule"},
		{"job", `zemi.job.wait({ cmd = "touch", args = { "MARKER" } })`, "zemi.job.wait", "job.wait"},
		{"mcp", `zemi.mcp.add("x", { command = "true" })`, "zemi.mcp.add", "mcp.add"},
		{"model call", `zemi.model.ask({ prompt = "x" })`, "zemi.model.ask", "model.ask"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := h.runModelLua(t.Context(), c.script, luaToolChunk); err == nil || !strings.Contains(err.Error(), "asks before zemi."+c.member) {
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
	for ns, ms := range bootedNamespaces(t, false) {
		for _, m := range ms {
			if name := strings.TrimPrefix(ns, "zemi.") + "." + m; !luaReadOnly[name] && !luaDelegated[name] {
				members = append(members, fmt.Sprintf("%q", name))
			}
		}
	}
	out, err := h.runModelLua(t.Context(), `local open, n = {}, 0
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
	t.Cleanup(func() { luaToolCeiling = old })
	tool := h.luaEvalTool(h.PipelineFor())
	for _, c := range []struct {
		script, want string
		ceiling      time.Duration
		named        bool // the error names the chunk the run got
	}{
		{"while true do end", "error: stopped after its 50ms ceiling", 50 * time.Millisecond, false},
		{"error({ code = 1 })", `: {"code":1}`, old, true},
	} {
		luaToolCeiling = c.ceiling
		input, _ := json.Marshal(map[string]string{"about": "x", "script": c.script}, json.Deterministic(true))
		resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "1", Name: "lua", Input: string(input)})
		if err != nil || !resp.IsError || !strings.Contains(resp.Content, c.want) ||
			(c.named && !strings.Contains(resp.Content, "error: "+luaToolChunk+"#")) || strings.Contains(resp.Content, "context canceled") {
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
	outside := filepath.Join(outsideTemp(t), "x.lua")
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
		input, _ := json.Marshal(map[string]string{"about": "x", "path": c.path}, json.Deterministic(true))
		resp, _ := h.luaEvalTool(h.PipelineFor()).Run(t.Context(), fantasy.ToolCall{ID: "1", Name: "lua", Input: string(input)})
		if !strings.HasPrefix(resp.Content, c.want) || len(ui.asked) != c.asks {
			t.Errorf("%s: %q, %d asks", c.path, resp.Content, len(ui.asked))
		}
	}
}

// TestLuaToolReloads: a script runs on the config on disk, reloaded first
// when it moved, and the result leads with how the reload went.
func TestLuaToolReloads(t *testing.T) {
	for _, c := range []struct {
		name    string
		changed bool
		step    func(h *Host) error // a boot step of the reload; nil fails before the replay
		want    string
	}{
		{"nothing changed, no reload", false, nil, "return: true"},
		{"a change reloads before the script", true, func(h *Host) error {
			_, err := h.Settings.SetQuiet("ui.mouse", false)
			return err
		}, "(zemi reloaded the changed config)\nreturn: false"},
		{"errors the reload recorded are listed", true, func(h *Host) error {
			h.RecordError("boot plugins", "plugin x: init.lua:3: boom")
			h.RecordError("render", "not the reload's")
			return nil
		}, "(zemi reloaded the changed config; 1 error)\nerror: boot plugins: plugin x: init.lua:3: boom\nreturn: true"},
		{"a replay that stopped says why", true, func(*Host) error {
			return errors.New("config: boom")
		}, "(zemi reloaded the changed config; 1 error)\nerror: boot step config: config: boom\nerror: zemi: lua runtime is hot, reload required"},
		{
			"a reload that kept the generation says the old config runs", true, nil,
			"(the changed config did not load; the old one still runs)\nerror: init.lua:3: unexpected symbol\nreturn: true",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestHost(t)
			h.SetProjectTrusted(true)
			fakeSandbox(h, true)
			if err := h.SetPermMode("auto"); err != nil {
				t.Fatal(err)
			}
			changed := c.changed
			h.SetReload(func() error {
				changed = false
				if c.step == nil {
					return errors.New("init.lua:3: unexpected symbol")
				}
				step := lua.BootStep{Name: "config", Run: func(context.Context, *lua.Runtime, *lua.Generation) error { return c.step(h) }}
				return h.ForceReload(t.Context(), []lua.BootStep{h.NamespacesStep(), step})
			}, func() bool { return changed })
			input, _ := json.Marshal(map[string]string{"about": "x", "script": `return zemi.config.get("ui.mouse")`}, json.Deterministic(true))
			resp, err := h.luaEvalTool(h.PipelineFor()).Run(t.Context(), fantasy.ToolCall{ID: "1", Name: "lua", Input: string(input)})
			if err != nil || resp.Content != c.want || changed {
				t.Fatalf("%q (%v), still changed %v", resp.Content, err, changed)
			}
		})
	}
}

// TestLuaToolDrivesTools: a script calls the run's own tools through the
// pipeline a direct call takes, only what the script prints comes back, and a
// script that only drives tools asks nobody.
func TestLuaToolDrivesTools(t *testing.T) {
	h, _ := newTestHost(t)
	h.SetProjectTrusted(true)
	fakeSandbox(h, true)
	if err := h.SetPermMode("auto"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(h.Dirs.ProjectDir, "a.txt")
	if err := os.WriteFile(file, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ui := &fakeUI{askAllowed: false, askBy: "user"}
	h.SetUIHooks(ui)
	set, err := h.profileTools(nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool fantasy.AgentTool
	for _, c := range set {
		if c.Info().Name == tools.ToolLua {
			tool = c
		}
	}
	if tool == nil {
		t.Fatal("the set has no lua tool")
	}
	run := func(script string) string {
		t.Helper()
		input, _ := json.Marshal(map[string]string{"about": "x", "script": script}, json.Deterministic(true))
		resp, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "1", Name: tools.ToolLua, Input: string(input)})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Content
	}
	// the read runs, its output is the script's to keep, and the script itself
	// asked nobody: the call decides for itself
	read := `"` + file + `", limit = 0`
	if out := run(`local o, e = zemi.tool.call("read", { path = ` + read + ` }):wait() print(e or o)`); !strings.Contains(out, "hello") || len(ui.asked) != 0 {
		t.Fatalf("read through a script: %q, asked %+v", out, ui.asked)
	}
	// a nested read nobody prints stays out of the model's context
	if out := run(`zemi.tool.call("read", { path = ` + read + ` }):wait()`); out != "(no output)" {
		t.Fatalf("unprinted output reached the model: %q", out)
	}
	// a shell command runs, and its output comes back whole
	if out := run(`local o, e = zemi.tool.call("shell", { command = "cat ` + file + `" }):wait() print(e or o)`); !strings.Contains(out, "hello") || len(ui.asked) != 0 {
		t.Fatalf("shell through a script: %q, asked %+v", out, ui.asked)
	}
	// a call the gate asks about asks once, says the script asked, and denies
	// the script's wait when the user says no
	out := run(`local o, e = zemi.tool.call("shell", { command = "rm -rf build" }):wait() print(e or o)`)
	if len(ui.asked) != 1 || ui.asked[0].Script != "lua script" || ui.asked[0].Subject != perm.SubjectShell {
		t.Fatalf("nested ask: %+v", ui.asked)
	}
	if !strings.Contains(out, "permission denied") || strings.Contains(out, "rm -rf") {
		t.Fatalf("denied call: %q", out)
	}
	// an unknown tool and unusable arguments come back as nil, err; nothing runs
	ui.asked = nil
	if out := run(`local h, e = zemi.tool.call("nope", {}) print(h == nil, e ~= nil)`); out != "true\ttrue" {
		t.Fatalf("unknown tool: %q", out)
	}
	if out := run(`local h, e = zemi.tool.call("read", "not a table") print(h == nil, e ~= nil)`); out != "true\ttrue" || len(ui.asked) != 0 {
		t.Fatalf("bad args: %q, asked %+v", out, ui.asked)
	}
	// describe names a tool of this run, and nothing for one it has not
	if out := run(`local d = zemi.tool.describe("read") return d.name, d.input.path ~= nil, zemi.tool.describe("nope") == nil`); !strings.Contains(out, `"read",true,true`) {
		t.Fatalf("describe: %q", out)
	}
	// a lua tool is not in the set: no script starts another script
	if out := run(`local h, e = zemi.tool.call("lua", { about = "x", script = "print(1)" }) print(h == nil, e ~= nil)`); out != "true\ttrue" {
		t.Fatalf("nested lua tool: %q", out)
	}
	// outside a running script the member refuses, so config and callbacks
	// cannot drive tools
	if out, err := h.runModelLua(t.Context(), `print(zemi.tool.call("read", { path = "x" }))`, luaToolChunk); err != nil || !strings.Contains(out, "while it runs") {
		t.Fatalf("call outside a script: %q %v", out, err)
	}
}

// TestLuaCallsCap: at most the cap's calls run at once, a call waiting for a
// slot enters as soon as one frees, and one a canceled script waits on never
// enters.
func TestLuaCallsCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newLuaCalls(2)
		first, ok := c.enter(t.Context())
		if !ok || first == nil {
			t.Fatal("the first call did not enter")
		}
		second, ok := c.enter(t.Context())
		if !ok || second == nil {
			t.Fatal("the second call did not enter")
		}
		entered := make(chan struct{})
		go func() {
			if leave, ok := c.enter(t.Context()); ok {
				close(entered)
				leave()
			}
		}()
		synctest.Wait() // the cap is full: it waits, and stays there
		select {
		case <-entered:
			t.Fatal("a call entered past the cap")
		default:
		}
		first()
		select {
		case <-entered:
		case <-time.After(10 * time.Second): // virtual: only a bug reaches it
			t.Fatal("a waiting call did not enter after a slot freed")
		}
		second()
		// a canceled script's waiting call does not enter
		ctx, cancel := context.WithCancel(t.Context())
		heldA, okA := c.enter(ctx)
		heldB, okB := c.enter(ctx)
		if !okA || !okB {
			t.Fatal("the cap did not take both calls")
		}
		waiting := make(chan bool, 1)
		go func() {
			_, ok := c.enter(ctx)
			waiting <- ok
		}()
		synctest.Wait()
		cancel()
		if ok := <-waiting; ok {
			t.Fatal("a call entered after the script was canceled")
		}
		heldA()
		heldB()
	})
}
