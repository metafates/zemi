package hostapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// TestToolDeniedEvent: a call the gate refuses reaches tool.denied
// handlers with the tool, subject and deciding source.
func TestToolDeniedEvent(t *testing.T) {
	h, r := newTestHostDB(t)
	runUser(t, r, `zemi.event.on("tool.denied", function(ev)
		zemi.state.set("denied", ev.tool .. " " .. ev.subject .. " " .. ev.by)
	end)`)
	h.Perm.Add(perm.Rule{Subject: "fs.read", Pattern: "*secret*", Decision: perm.Deny})
	for _, tl := range tools.All(h.PipelineFor()) {
		if tl.Info().Name != "read" {
			continue
		}
		resp, err := tl.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "read", Input: `{"path":"/p/secret.txt","limit":0}`})
		if err != nil || !resp.IsError {
			t.Fatalf("denied read = %+v, %v", resp, err)
		}
	}
	waitFor(t, func() error {
		if v, _ := h.GlobalDB.GetKV("denied"); v != `"read fs.read rule"` {
			return fmt.Errorf("tool.denied payload = %s", v)
		}
		return nil
	})
}

// TestHostToolEvents: a host-implemented tool gates and runs outside
// internal/tools, so it has to fire tool.denied and tool.post itself.
func TestHostToolEvents(t *testing.T) {
	h, r := newTestHostDB(t)
	runUser(t, r, `zemi.event.on("tool.denied", function(ev)
		zemi.state.set("wdenied", ev.tool .. " " .. ev.subject)
	end)
	zemi.event.on("tool.post", function(ev)
		zemi.state.set("wpost", tostring(ev.ok))
	end)`)
	h.Perm.Add(perm.Rule{Subject: "tool.workflow", Pattern: "*", Decision: perm.Deny})
	resp, err := h.workflowTool().Run(context.Background(),
		fantasy.ToolCall{ID: "1", Name: "workflow", Input: `{"description":"d","script":"return 1"}`})
	if err != nil || !resp.IsError {
		t.Fatalf("denied workflow = %+v, %v", resp, err)
	}
	waitFor(t, func() error {
		denied, _ := h.GlobalDB.GetKV("wdenied")
		post, _ := h.GlobalDB.GetKV("wpost")
		if denied != `"workflow tool.workflow"` || post != `"false"` {
			return fmt.Errorf("denied = %s, post = %s", denied, post)
		}
		return nil
	})
}

// TestEmitKeepsNameAndFrom: a payload cannot rename an event or claim
// another origin, and a user handler a project emission runs inline runs as
// the project.
func TestEmitKeepsNameAndFrom(t *testing.T) {
	h, r := newTestHostDB(t)
	runUser(t, r, `zemi.event.on("user.y", function(ev) zemi.state.set("ev", ev.from .. " " .. ev.name) end)`)
	if err := r.ExecuteChunk(t.Context(), `zemi.event.emit("user.y", { from = "host", name = "x" })`, "p.lua", lua.OriginProject); err != nil {
		t.Fatal(err)
	}
	if v, _ := h.GlobalDB.GetKV("project:ev"); v != `"project user.y"` {
		t.Fatalf("handler saw %s", v)
	}
}

// TestLuaAgentsNeedUserConfig: project code cannot act with
// the user's authority from Lua.
func TestLuaAgentsNeedUserConfig(t *testing.T) {
	h, r := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	runUser(t, r, `zemi.command.add("usercmd", function() end)`)
	h.Commands.Add(&Command{Name: "gocmd", GoFn: func(*Host, string) error { return nil }, Origin: lua.OriginBuiltin})
	t.Setenv("ZEMI_TEST_API_KEY", "k")
	for _, src := range []string{
		`zemi.agent.run({ prompt = "x" })`, `zemi.workflow.run({ script = "return 1" })`,
		`zemi.agent.send("x")`, `zemi.agent.profile.add("evil", { system = "x" })`, `zemi.config.set("agent.profile", "evil")`,
		`zemi.composer.set("!touch x")`, `zemi.composer.insert("x")`, `zemi.composer.submit()`,
		`zemi.session.new()`, `zemi.session.resume("x")`, `zemi.session.rewind("x")`,
		`zemi.memory.add("x")`,
		`zemi.model.set("p/m")`, `zemi.config.reload()`, `assert(zemi.command.run("usercmd"))`,
		`zemi.ui.focus.set()`, `zemi.ui.focus.next()`, `zemi.ui.focus.prev()`,
		`zemi.ui.hl.set("Normal", { fg = "#ff0000" })`, `zemi.ui.theme.set("whorl")`,
		`zemi.keymap.set("<leader>y", "zemi.ui.focus.set side")`,
		`zemi.config.set("permissions.mode", "yolo")`, `zemi.provider.add({ id = "x", kind = "openai_compat" })`,
		`zemi.lsp.add("x", { command = "sh", filetypes = { "x" } })`, `zemi.lsp.del("gopls")`,
		`zemi.permission.mode.add("x", { base = "yolo" })`, `zemi.permission.mode.del("auto")`,
		`zemi.keymap.set("<leader>y", "zemi.permission.mode.next")`, `zemi.keymap.set("<leader>y", "zemi.agent.profile.next")`,
		`zemi.keymap.set("<leader>y", "zemi.agent.profile.prev")`, `assert(zemi.command.run("gocmd"))`,
		`zemi.http.request({ url = "http://x", headers = { Authorization = "t" } })`, `zemi.env.get("ZEMI_TEST_API_KEY")`,
	} {
		if err := r.ExecuteChunk(context.Background(), src, "p.lua", lua.OriginProject); err == nil || !strings.Contains(err.Error(), "only user config") && !strings.Contains(err.Error(), "may not") && !strings.Contains(err.Error(), "secret denylist") {
			t.Errorf("%s from project: %v", src, err)
		}
	}
	// a mode rule outside the subject namespaces would sit in its store and never match
	if err := r.ExecuteChunk(context.Background(), `zemi.permission.mode.add("x", { base = "auto", rules = { { "bash", "ls *", "allow" } } })`, "u.lua", lua.OriginUser); err == nil || !strings.Contains(err.Error(), `subject "bash" starts with none of`) {
		t.Fatalf("mode rule with a bare subject: %v", err)
	}
	runUser(t, r, `
		zemi.config.set("permissions.mode", "yolo")
		zemi.permission.mode.add("x", { base = "auto", rules = { { "tool.bash", "ls *", "allow" } } })
		zemi.permission.mode.del("x")
		zemi.lsp.del("gopls")
		zemi.keymap.set("<leader>y", "zemi.permission.mode.next")
		assert(zemi.command.run("gocmd"))
		assert(zemi.env.get("ZEMI_TEST_API_KEY") == "k")`)
	if err := r.ExecuteChunk(context.Background(), `zemi.event.on("app.quit", function() return { decision = "deny" } end)`, "p.lua", lua.OriginProject); err != nil {
		t.Fatal(err)
	}
	if res := h.EmitVetoable(context.Background(), "app.quit", map[string]any{}, lua.VetoDeadline); res.Denied {
		t.Errorf("project app.quit deny = %+v, want ignored", res)
	}
}

// TestBuiltinReviewWorkflow: the shipped review workflow is listed, loads
// in the workflow sandbox and runs to a result.
func TestBuiltinReviewWorkflow(t *testing.T) {
	h, _ := newTestHostDB(t)
	var review *Workflow
	for _, w := range h.workflows() {
		if w.Name == "review" {
			review = &w
		}
	}
	if review == nil || review.Source != "builtin" || review.Description == "" {
		t.Fatalf("review workflow = %+v", review)
	}
	src, err := h.workflowSource("review")
	if err != nil {
		t.Fatal(err)
	}
	echoAgents(h)
	res, err := h.runWorkflow(context.Background(), src, nil, nil)
	if err != nil || !strings.HasPrefix(res.Value, "Verify these review findings") {
		t.Fatalf("review run = %q, %v", res.Value, err)
	}
}

func TestToolPreOnce(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `zemi.event.on("tool.pre", function() return { decision = "deny", reason = "once" } end, { once = true })`)
	pre := h.toolPreHook()
	for i, want := range []string{"deny", ""} {
		if d, _, _, _ := pre("bash", map[string]any{"command": "ls"}); d != want {
			t.Fatalf("call %d: decision %q, want %q", i+1, d, want)
		}
	}
}

// TestToolPostOnceOverlap: two tool calls whose tool.post runs overlap each
// hold the handler list, and a once handler still runs for one of them.
func TestToolPostOnceOverlap(t *testing.T) {
	h, r := newTestHost(t)
	var both sync.WaitGroup
	both.Add(2)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		both.Done()
		both.Wait() // both calls are past their Handlers copy
	}))
	defer srv.Close()
	runUser(t, r, fmt.Sprintf(`
		zemi.permission.rule.add({ subject = "lua.http.get", pattern = "%[1]s*", decision = "allow", match = "%[1]s/x" })
		zemi.event.on("tool.post", function() zemi.http.get("%[1]s/x"):wait() end)
		zemi.event.on("tool.post", function(ev) return { output = ev.output .. " once" } end, { once = true })`, srv.URL))
	outs := make([]string, 2)
	var wg sync.WaitGroup
	for i := range outs {
		wg.Go(func() { outs[i] = h.toolPost(t.Context(), "echo", map[string]any{}, true, "x", nil, 0) })
	}
	wg.Wait()
	if n := strings.Count(strings.Join(outs, ","), "once"); n != 1 {
		t.Fatalf("outputs %q: the once handler ran %d times", outs, n)
	}
}

// TestVetoShape pins the zemi.Veto table runtime/types/zemi.lua documents.
func TestVetoShape(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.event.on("permission.request", function() return { decision = "deny", reason = "nope" } end)
		zemi.event.on("tool.pre", function() return { decision = "halt", reason = "stop" } end)`)
	if res := h.EmitVetoable(t.Context(), "permission.request", map[string]any{}, lua.VetoDeadline); !res.Denied || res.Reason != "nope" {
		t.Errorf("permission.request = %+v, want denied with reason nope", res)
	}
	if d, reason, _, _ := h.toolPreHook()("bash", map[string]any{"command": "ls"}); d != "halt" || reason != "stop" {
		t.Errorf("tool.pre = %q %q, want halt stop", d, reason)
	}
}

// TestVetoBadShapeFailsClosed: a handler that meant to decide and wrote
// the wrong shape must not read as consent where the event has a closed
// side (DESIGN §6.0).
func TestVetoBadShapeFailsClosed(t *testing.T) {
	for _, ret := range []string{`"deny"`, `true`, `{ decision = "block" }`} {
		t.Run(ret, func(t *testing.T) {
			h, r := newTestHost(t)
			runUser(t, r, `
				zemi.event.on("permission.request", function() return `+ret+` end)
				zemi.event.on("app.quit", function() return `+ret+` end)
				zemi.event.on("tool.pre", function() return `+ret+` end)`)
			if res := h.EmitVetoable(t.Context(), "permission.request", map[string]any{}, lua.VetoDeadline); !res.Denied {
				t.Errorf("permission.request = %+v, want denied", res)
			}
			if res := h.EmitVetoable(t.Context(), "app.quit", map[string]any{}, lua.VetoDeadline); res.Denied {
				t.Errorf("app.quit = %+v, want proceed", res)
			}
			if d, _, _, _ := h.toolPreHook()("bash", map[string]any{"command": "ls"}); d != "deny" {
				t.Errorf("tool.pre = %q, want deny", d)
			}
		})
	}
}

// TestHotRuntimeVetoesFailClosed: while the runtime is hot the deny hooks
// cannot run, so permission.request and tool.pre deny and app.quit proceeds.
func TestHotRuntimeVetoesFailClosed(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.event.on("permission.request", function() end)
		zemi.event.on("app.quit", function() end)
		zemi.event.on("tool.pre", function() end)`)
	if _, err := r.Do(context.Background(), lua.ClassIO, 10*time.Millisecond, 10*time.Millisecond, func(*glua.LState) ([]any, error) {
		time.Sleep(200 * time.Millisecond)
		return nil, nil
	}); !r.IsHot() {
		t.Fatalf("runtime not hot: %v", err)
	}
	if res := h.EmitVetoable(context.Background(), "permission.request", map[string]any{}, lua.VetoDeadline); !res.Denied {
		t.Errorf("permission.request while hot = %+v, want denied", res)
	}
	if res := h.EmitVetoable(context.Background(), "app.quit", map[string]any{}, lua.VetoDeadline); res.Denied {
		t.Errorf("app.quit while hot = %+v, want proceed", res)
	}
	if d, _, _, _ := h.toolPreHook()("bash", map[string]any{"command": "ls"}); d != "deny" {
		t.Errorf("tool.pre while hot = %q, want deny", d)
	}
}

func TestToolPreRewrite(t *testing.T) {
	const denyRm = `zemi.event.on("tool.pre", function(ev) if ev.input.command:match("^rm") then return { decision = "deny", reason = "no rm" } end end)`
	const rewriteRm = `zemi.event.on("tool.pre", function() return { decision = "rewrite", input = { command = "rm -rf x" } } end)`
	for _, c := range []struct {
		name         string
		origin       lua.Origin
		src          string
		wantDecision string
		wantCommand  string
	}{
		{"user rewrite applies", lua.OriginUser, rewriteRm, "rewrite", "rm -rf x"},
		{"project rewrite ignored", lua.OriginProject, rewriteRm, "", ""},
		{"rewrite rechecked by an earlier deny", lua.OriginUser, denyRm + "\n" + rewriteRm, "deny", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, r := newTestHost(t)
			if err := r.ExecuteChunk(context.Background(), c.src, "c.lua", c.origin); err != nil {
				t.Fatal(err)
			}
			d, _, rw, _ := h.toolPreHook()("bash", map[string]any{"command": "ls"})
			cmd, _ := rw["command"].(string)
			if d != c.wantDecision || cmd != c.wantCommand {
				t.Fatalf("decision %q command %q, want %q %q", d, cmd, c.wantDecision, c.wantCommand)
			}
		})
	}
}

// TestLuaToolRunsToolPre: a rewrite is refused rather than running the
// original input the hook meant to replace.
func TestLuaToolRunsToolPre(t *testing.T) {
	for _, c := range []struct{ ret, want string }{
		{`{ decision = "deny", reason = "no" }`, "denied by hook"},
		{`{ decision = "rewrite", input = { city = "x" } }`, "rewrite is not supported"},
	} {
		h, r := newTestHost(t)
		runUser(t, r, `
			zemi.tool.add({ name = "weather", description = "the weather", execute = function() return "sunny" end })
			zemi.event.on("tool.pre", function(ev) if ev.tool == "weather" then return `+c.ret+` end end)
		`)
		resp, err := h.luaFantasyTools(h.PipelineFor())[0].Run(context.Background(), fantasy.ToolCall{Input: "{}"})
		if err != nil || !resp.IsError || !strings.Contains(resp.Content, c.want) {
			t.Errorf("%s: resp = %+v, %v", c.ret, resp, err)
		}
	}
}
