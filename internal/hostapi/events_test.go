package hostapi

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/tools"
)

// TestToolDeniedEvent: a call the gate refuses reaches tool.denied
// handlers with the tool, subject and deciding source.
func TestToolDeniedEvent(t *testing.T) {
	h, r := newTestHostDB(t)
	runUser(t, r, `zemi.event.on("tool.denied", function(ev)
		zemi.state.set("denied", ev.tool .. " " .. ev.subject .. " " .. ev.by)
	end)`)
	if err := h.SetPermMode("ask"); err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.All(h.PipelineFor()) {
		if tl.Info().Name != "write" {
			continue
		}
		resp, err := tl.Run(t.Context(), fantasy.ToolCall{ID: "1", Name: "write", Input: `{"path":"/p/secret.txt","content":"x"}`})
		if err != nil || !resp.IsError {
			t.Fatalf("denied write = %+v, %v", resp, err)
		}
	}
	waitFor(t, func() error {
		if v, _ := h.GlobalDB.GetKV("denied"); v != `"write fs.write nobody"` {
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
	if err := h.SetPermMode("ask"); err != nil {
		t.Fatal(err)
	}
	resp, err := h.workflowTool(h.PipelineFor()).Run(t.Context(),
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

// TestHostEmitsAreCatalogued reads the source, since most host emits run
// under a recover (GoSafe, a turn, a Lua call) that would swallow a failure.
func TestHostEmitsAreCatalogued(t *testing.T) {
	nameArg := map[string]int{"EmitAsync": 0, "EmitVetoable": 1, "emitVetoable": 1, "EmitOnState": 1, "recordEvent": 0}
	files, _ := filepath.Glob("*.go")
	for _, dir := range []string{"../ui", "../.."} {
		more, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		files = append(files, more...)
	}
	seen := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var fn string
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				fn = fun.Name
			case *ast.SelectorExpr:
				fn = fun.Sel.Name
			}
			i, ok := nameArg[fn]
			if !ok || len(call.Args) <= i {
				return true
			}
			lit, ok := call.Args[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if fn == "recordEvent" {
				if from, ok := call.Args[1].(*ast.BasicLit); !ok || from.Value != `"host"` {
					return true
				}
			}
			name, _ := strconv.Unquote(lit.Value)
			if seen++; !eventCatalogue[name] {
				t.Errorf("%s: %s(%q) is not in eventCatalogue", file, fn, name)
			}
			return true
		})
	}
	if seen == 0 {
		t.Fatal("found no host emits")
	}
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
	if err := r.ExecuteChunk(t.Context(), `zemi.event.emit("user.y", { "a", "b" })`, "p.lua", lua.OriginProject); err == nil {
		t.Fatal("a sequence payload did not raise")
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
	for _, name := range []string{"mode", "profile"} {
		h.Commands.Add(&Command{Name: name, GoFn: func(*Host, string) error { return nil }, Origin: lua.OriginBuiltin})
	}
	t.Setenv("ZEMI_TEST_API_KEY", "k")
	for _, src := range []string{
		`zemi.agent.run({ prompt = "x" })`, `zemi.workflow.run({ script = "return 1" })`,
		`zemi.agent.send("x")`, `zemi.agent.profile.add("evil", { system = "x" })`, `zemi.config.set("agent.profile", "evil")`,
		`zemi.composer.set("!touch x")`, `zemi.composer.insert("x")`, `zemi.composer.submit()`,
		`zemi.session.new()`, `zemi.session.resume("x")`, `zemi.session.rewind("x")`,
		`zemi.memory.add("x")`,
		`zemi.model.set("p/m")`, `assert(zemi.command.run("usercmd"))`,
		`zemi.ui.focus.set()`, `zemi.ui.focus.next()`, `zemi.ui.focus.prev()`,
		`zemi.ui.hl.set("Normal", { fg = "#ff0000" })`,
		`zemi.keymap.set("<leader>y", "zemi.ui.focus.set side")`,
		`zemi.config.set("permissions.mode", "ask")`, `zemi.provider.add({ id = "x", kind = "openai_compat" })`,
		`zemi.lsp.add("x", { command = "sh", filetypes = { "x" } })`, `zemi.lsp.del("gopls")`,
		`zemi.keymap.set("<leader>y", "zemi.permission.mode.next")`, `zemi.keymap.set("<leader>y", "zemi.agent.profile.next")`,
		`zemi.keymap.set("<leader>y", "zemi.agent.profile.prev")`, `assert(zemi.command.run("gocmd"))`,
		`zemi.keymap.set("<leader>y", "mode")`, `zemi.keymap.set("<leader>y", "profile")`,
		`zemi.http.request({ url = "http://x", headers = { Authorization = "t" } })`, `zemi.env.get("ZEMI_TEST_API_KEY")`,
	} {
		if err := r.ExecuteChunk(t.Context(), src, "p.lua", lua.OriginProject); err == nil || !strings.Contains(err.Error(), "only user config") && !strings.Contains(err.Error(), "may not") && !strings.Contains(err.Error(), "secret denylist") {
			t.Errorf("%s from project: %v", src, err)
		}
	}
	runUser(t, r, `
		zemi.config.set("permissions.mode", "ask")
		zemi.lsp.del("gopls")
		zemi.keymap.set("<leader>y", "zemi.permission.mode.next")
		assert(zemi.command.run("gocmd"))
		assert(zemi.env.get("ZEMI_TEST_API_KEY") == "k")`)
	if err := r.ExecuteChunk(t.Context(), `zemi.event.on("app.quit", function() return { decision = "deny" } end)`, "p.lua", lua.OriginProject); err != nil {
		t.Fatal(err)
	}
	if res := h.EmitVetoable(t.Context(), "app.quit", map[string]any{}); res.Denied {
		t.Errorf("project app.quit deny = %+v, want ignored", res)
	}
}

// TestBuiltinWorkflows: every shipped workflow is listed, loads in the
// workflow sandbox and runs to a result.
func TestBuiltinWorkflows(t *testing.T) {
	h, _ := newTestHostDB(t)
	echoAgents(h)
	for _, c := range []struct{ name, args, want string }{
		{"review", "", "Verify these review findings"},
		{"research", "what is a whorl", "Answer the question below"},
		{"hunt", "the spinner never stops", "Decide the cause of this failure"},
	} {
		i := slices.IndexFunc(h.workflows(), func(w Workflow) bool { return w.Name == c.name })
		if i < 0 || h.workflows()[i].Source != lua.OriginBuiltin || h.workflows()[i].Description == "" {
			t.Fatalf("%s workflow = %+v", c.name, h.workflows())
		}
		src, err := h.workflowSource(c.name)
		if err != nil {
			t.Fatal(err)
		}
		res, err := runFlow(t.Context(), h, src, c.args)
		if err != nil || !strings.HasPrefix(res, c.want) {
			t.Fatalf("%s run = %q, %v", c.name, res, err)
		}
	}
}

func TestToolPreOnce(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `zemi.event.on("tool.pre", function() return { decision = "deny", reason = "once" } end, { once = true })`)
	pre := h.toolPreHook()
	for i, want := range []tools.PreVerdict{tools.PreDeny, ""} {
		if d, _, _ := pre("shell", map[string]any{"command": "ls"}); d != want {
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
		zemi.event.on("tool.post", function() zemi.http.get("%[1]s/x"):wait() end)
		zemi.event.on("tool.post", function(ev) return { output = ev.output .. " once" } end, { once = true })`, srv.URL))
	outs := make([]string, 2)
	var wg sync.WaitGroup
	for i := range outs {
		wg.Go(func() { outs[i] = h.toolPost(t.Context(), "", "echo", map[string]any{}, true, "x", nil, 0) })
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
	if res := h.EmitVetoable(t.Context(), "permission.request", map[string]any{}); !res.Denied || res.Reason != "nope" {
		t.Errorf("permission.request = %+v, want denied with reason nope", res)
	}
	if d, reason, _ := h.toolPreHook()("shell", map[string]any{"command": "ls"}); d != "halt" || reason != "stop" {
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
			if res := h.EmitVetoable(t.Context(), "permission.request", map[string]any{}); !res.Denied {
				t.Errorf("permission.request = %+v, want denied", res)
			}
			if res := h.EmitVetoable(t.Context(), "app.quit", map[string]any{}); res.Denied {
				t.Errorf("app.quit = %+v, want proceed", res)
			}
			if d, _, _ := h.toolPreHook()("shell", map[string]any{"command": "ls"}); d != "deny" {
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
	if _, err := r.Do(t.Context(), lua.ClassIO, 10*time.Millisecond, 10*time.Millisecond, func(*glua.LState) ([]any, error) {
		<-t.Context().Done()
		return nil, nil
	}); !r.IsHot() {
		t.Fatalf("runtime not hot: %v", err)
	}
	if res := h.EmitVetoable(t.Context(), "permission.request", map[string]any{}); !res.Denied {
		t.Errorf("permission.request while hot = %+v, want denied", res)
	}
	if res := h.EmitVetoable(t.Context(), "app.quit", map[string]any{}); res.Denied {
		t.Errorf("app.quit while hot = %+v, want proceed", res)
	}
	if d, _, _ := h.toolPreHook()("shell", map[string]any{"command": "ls"}); d != "deny" {
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
		wantDecision tools.PreVerdict
		wantCommand  string
	}{
		{"user rewrite applies", lua.OriginUser, rewriteRm, "rewrite", "rm -rf x"},
		{"project rewrite ignored", lua.OriginProject, rewriteRm, "", ""},
		{"rewrite rechecked by an earlier deny", lua.OriginUser, denyRm + "\n" + rewriteRm, "deny", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, r := newTestHost(t)
			if err := r.ExecuteChunk(t.Context(), c.src, "c.lua", c.origin); err != nil {
				t.Fatal(err)
			}
			d, _, rw := h.toolPreHook()("shell", map[string]any{"command": "ls"})
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
		resp, err := h.luaFantasyTools(h.PipelineFor())[0].Run(t.Context(), fantasy.ToolCall{Input: "{}"})
		if err != nil || !resp.IsError || !strings.Contains(resp.Content, c.want) {
			t.Errorf("%s: resp = %+v, %v", c.ret, resp, err)
		}
	}
}
