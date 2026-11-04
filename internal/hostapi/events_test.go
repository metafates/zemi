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
	askEverything(t, r)
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
	askEverything(t, r)
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
// An emit names its event as a literal or as a constant, which is resolved
// from the same source.
func TestHostEmitsAreCatalogued(t *testing.T) {
	nameArg := map[string]int{"EmitAsync": 0, "EmitVetoable": 1, "emitVetoable": 1, "EmitOnState": 1, "recordEvent": 0}
	files, _ := filepath.Glob("*.go")
	for _, dir := range []string{"../ui", "../.."} {
		more, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		files = append(files, more...)
	}
	fset := token.NewFileSet()
	var parsed []*ast.File
	consts := map[string]string{}
	lit := func(e ast.Expr) (string, bool) {
		b, ok := e.(*ast.BasicLit)
		if !ok || b.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(b.Value)
		return s, err == nil
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, f)
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					t.Fatalf("const spec = %T, want a value spec", spec)
				}
				for i, n := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if v, ok := lit(vs.Values[i]); ok {
						consts[n.Name] = v
					}
				}
			}
		}
	}
	// name resolves a literal or a const ident, of this package or a
	// selected one (hostapi.EventToolPre): false leaves a variable alone.
	name := func(e ast.Expr) (string, bool) {
		if b, ok := lit(e); ok {
			return b, true
		}
		var id string
		switch e := e.(type) {
		case *ast.Ident:
			id = e.Name
		case *ast.SelectorExpr:
			id = e.Sel.Name
		default:
			return "", false
		}
		s, ok := consts[id]
		return s, ok
	}
	seen := 0
	for _, f := range parsed {
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
			if fn == "recordEvent" {
				if from, ok := call.Args[1].(*ast.BasicLit); !ok || from.Value != `"host"` {
					return true
				}
			}
			ev, ok := name(call.Args[i])
			if !ok {
				return true
			}
			if seen++; !_eventCatalogue.Has(ev) {
				t.Errorf("%s: %s(%q) is not in eventCatalogue", fset.Position(call.Pos()).Filename, fn, ev)
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
// its own file.
func TestEmitKeepsNameAndFrom(t *testing.T) {
	h, r := newTestHostDB(t)
	runUser(t, r, `zemi.event.on("user.y", function(ev) zemi.state.set("ev", ev.from .. " " .. ev.name) end)`)
	if err := r.ExecuteChunk(t.Context(), `zemi.event.emit("user.y", { from = "host", name = "x" })`, "p.lua", lua.OriginProject); err != nil {
		t.Fatal(err)
	}
	if err := r.ExecuteChunk(t.Context(), `zemi.event.emit("user.y", { "a", "b" })`, "p.lua", lua.OriginProject); err == nil {
		t.Fatal("a sequence payload did not raise")
	}
	if v, _ := h.GlobalDB.GetKV("ev"); v != `"project user.y"` {
		t.Fatalf("handler saw %s", v)
	}
}

// TestBuiltinWorkflows: every shipped workflow is listed, loads in the
// workflow sandbox and runs to a result.
func TestBuiltinWorkflows(t *testing.T) {
	h, _ := newTestHostDB(t)
	echoAgents(h)
	for _, c := range []struct{ name, args, want string }{
		{"review", "", "Verify these review findings"},
		{"hunt", "the spinner never stops", "Decide the cause of this failure"},
		{"brainstorm", "fewer roundtrips", "Turn this brainstorm into the worth-try list"},
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

// TestVetoCrashFailsClosed: a handler that raises instead of returning is
// the bad shape's sibling - permission.request denies on it, an event with
// no closed side keeps its default.
func TestVetoCrashFailsClosed(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.event.on("permission.request", function() error("boom") end)
		zemi.event.on("app.quit", function() error("boom") end)`)
	if res := h.EmitVetoable(t.Context(), EventPermissionRequest, map[string]any{}); !res.Denied || !strings.Contains(res.Reason, "boom") {
		t.Errorf("permission.request = %+v, want denied with the crash as reason", res)
	}
	if res := h.EmitVetoable(t.Context(), EventAppQuit, map[string]any{}); res.Denied {
		t.Errorf("app.quit = %+v, want proceed", res)
	}
}

// TestVetoAskLeg: a permission.request handler may return { decision =
// "ask" } to run the ask path; a denial from any handler outranks it.
func TestVetoAskLeg(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.event.on("permission.request", function(ev)
			if ev.subject == "tool.shell" then return { decision = "ask", reason = "nervous" } end
			return nil
		end)`)
	if res := h.EmitVetoable(t.Context(), EventPermissionRequest, map[string]any{"subject": "tool.shell"}); !res.Ask || res.Denied {
		t.Errorf("= %+v, want ask", res)
	}
	if res := h.EmitVetoable(t.Context(), EventPermissionRequest, map[string]any{"subject": "fs.read"}); res.Ask || res.Denied {
		t.Errorf("= %+v, want no opinion", res)
	}
	runUser(t, r, `zemi.event.on("permission.request", function() return { decision = "deny", reason = "no" } end, { id = "denier" })`)
	res := h.EmitVetoable(t.Context(), EventPermissionRequest, map[string]any{"subject": "tool.shell"})
	if !res.Denied || res.Ask {
		t.Errorf("= %+v, want the denial to outrank the ask", res)
	}
	if res := h.EmitVetoable(t.Context(), EventAppQuit, map[string]any{}); res.Denied || res.Ask {
		t.Errorf("app.quit = %+v, want proceed", res)
	}
}

// TestVetoReasonReachesTheDenial: the reason a permission.request handler
// denies with rides the verdict the gate logs and the denial text the model
// gets, truncated like the gate's other text.
func TestVetoReasonReachesTheDenial(t *testing.T) {
	for _, c := range []struct {
		name   string
		reason string
		want   string
	}{
		{"the handler's reason rides the denial", "edited", "permission denied (event): tool.shell: edited"},
		{"no reason reads as before", "", "permission denied (event): tool.shell"},
		{"a long reason truncates", strings.Repeat("x", 300), "permission denied (event): tool.shell: " + strings.Repeat("x", 200) + "..."},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, r := newTestHost(t)
			runUser(t, r, `zemi.event.on("permission.request", function()
				return { decision = "deny", reason = `+strconv.Quote(c.reason)+` }
			end)`)
			v := h.PipelineFor().Check(tools.Request{Tool: string(tools.ToolShell), Subject: perm.SubjectShell, Arg: "echo three"})
			if v.Allowed || v.DecidedBy != tools.ByEvent {
				t.Fatalf("Check = %+v, want an event denial", v)
			}
			if got := tools.DeniedText(v, perm.SubjectShell); got != c.want {
				t.Fatalf("denial text = %q, want %q", got, c.want)
			}
		})
	}
}

// TestVetoAtTheGate: the veto runs once per Check, the ask it settles does
// not run it again, and a host-API call from Lua (no asker) runs it never.
func TestVetoAtTheGate(t *testing.T) {
	h, r := newTestHostDB(t)
	seedSession(t, h)
	runUser(t, r, `
		zemi.event.on("permission.request", function()
			zemi.state.set("seen", (zemi.state.get("seen") or 0) + 1)
			return { decision = "ask" }
		end)`)
	v := h.PipelineFor().Check(tools.Request{Tool: string(tools.ToolShell), Subject: perm.SubjectShell, Arg: "ls"})
	if v.Allowed || v.DecidedBy != tools.ByNobody {
		t.Errorf("headless ask = %+v, want ByNobody", v)
	}
	runUser(t, r, `assert(zemi.state.get("seen") == 1, "the veto ran " .. zemi.state.get("seen") .. " times")`)
	runUser(t, r, `
		assert(zemi.fs.write("gate-runs.txt", "x"), "the Lua gate runs")
		assert(zemi.state.get("seen") == 1, "the Lua gate ran the veto")`)
}

// TestVetoCountedOnly: the gate's veto event with no handler listening
// counts but keeps no recent entry, or it would displace the log on every
// model action for the plugin-free majority.
func TestVetoCountedOnly(t *testing.T) {
	h, _ := newTestHost(t)
	if res := h.EmitVetoable(t.Context(), EventPermissionRequest, map[string]any{}); res.Denied || res.Ask {
		t.Errorf("= %+v, want silence", res)
	}
	h.evlog.mu.Lock()
	defer h.evlog.mu.Unlock()
	if h.evlog.counts[EventPermissionRequest] != 1 {
		t.Errorf("count = %d, want 1", h.evlog.counts[EventPermissionRequest])
	}
	for _, e := range h.evlog.entries {
		if e.name == EventPermissionRequest {
			t.Fatal("the handlerless veto kept a recent entry")
		}
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
	if _, err := r.Do(t.Context(), lua.ClassIO, 10*time.Millisecond, 10*time.Millisecond, "test", func(*glua.LState) ([]any, error) {
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
		{"a rewrite applies", lua.OriginUser, rewriteRm, "rewrite", "rm -rf x"},
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
