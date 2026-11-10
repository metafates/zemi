package hostapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/tools"
)

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
	pipe := h.PipelineFor()
	pipe.ForceAsk()
	resp, err := h.workflowTool(pipe).Run(t.Context(),
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
		{"hunt", "the spinner never stops", "Decide the cause of this failure"},
		{"brainstorm", "fewer roundtrips", "Turn this brainstorm into the worth-try list"},
		{"ship", "add a rate limiter", "ready to ship"},
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

// TestVetoBadShapeFailsClosed: a handler that meant to decide and wrote
// the wrong shape must not read as consent where the event has a closed
// side (DESIGN §6.0).
func TestVetoBadShapeFailsClosed(t *testing.T) {
	for _, ret := range []string{`"deny"`, `true`, `{ decision = "block" }`} {
		t.Run(ret, func(t *testing.T) {
			h, r := newTestHost(t)
			runUser(t, r, `
				zemi.event.on("app.quit", function() return `+ret+` end)
				zemi.event.on("tool.pre", function() return `+ret+` end)`)
			if res := h.EmitVetoable(t.Context(), EventAppQuit, map[string]any{}); res.Denied {
				t.Errorf("app.quit = %+v, want proceed", res)
			}
			if d, _, _ := h.toolPreHook()("shell", map[string]any{"command": "ls"}); d != "deny" {
				t.Errorf("tool.pre = %q, want deny", d)
			}
		})
	}
}

// TestVetoCrashFailsClosed: a handler that raises instead of returning
// keeps each event's default - tool.pre and app.quit proceed, the error
// lands in the log (tools.Tool.Run denies only when the whole hook cannot
// run, the hot runtime).
func TestVetoCrashFailsClosed(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.event.on("tool.pre", function() error("boom") end)
		zemi.event.on("app.quit", function() error("boom") end)`)
	if d, _, _ := h.toolPreHook()("shell", map[string]any{"command": "ls"}); d != "" {
		t.Errorf("tool.pre = %q, want proceed", d)
	}
	if res := h.EmitVetoable(t.Context(), EventAppQuit, map[string]any{}); res.Denied {
		t.Errorf("app.quit = %+v, want proceed", res)
	}
}

// TestHotRuntimeVetoesFailClosed: while the runtime is hot the deny hooks
// cannot run, so tool.pre denies and app.quit proceeds.
func TestHotRuntimeVetoesFailClosed(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.event.on("app.quit", function() end)
		zemi.event.on("tool.pre", function() end)`)
	if _, err := r.Do(t.Context(), lua.ClassIO, 10*time.Millisecond, 10*time.Millisecond, "test", func(*glua.LState) ([]any, error) {
		<-t.Context().Done()
		return nil, nil
	}); !r.IsHot() {
		t.Fatalf("runtime not hot: %v", err)
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
