package hostapi

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// TestToolDeniedEvent: a call the gate refuses reaches tool.denied
// handlers with the tool, subject and deciding source.
func TestToolDeniedEvent(t *testing.T) {
	h, r := newTestHostDB(t)
	runUser(t, r, `zemi.event.on("tool.denied", function(ev)
		zemi.state.put("denied", ev.tool .. " " .. ev.subject .. " " .. ev.by)
	end)`)
	h.Perm.Add(perm.Rule{Subject: "fs.read", Pattern: "*secret*", Decision: perm.Deny})
	for _, tl := range tools.All(h.PipelineFor()) {
		if tl.Info().Name != "read" {
			continue
		}
		resp, err := tl.Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "read", Input: `{"path":"/p/secret.txt"}`})
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

// TestLuaAgentsNeedUserConfig: project code cannot act with the user's
// authority from Lua.
func TestLuaAgentsNeedUserConfig(t *testing.T) {
	_, r := newTestHostDB(t)
	for _, src := range []string{
		`zemi.agent.run({ prompt = "x" })`, `zemi.workflow.run({ script = "return 1" })`,
		`zemi.agent.send("x")`, `zemi.agent.profile("evil", { system = "x" })`, `zemi.agent.set_default("default")`,
		`zemi.composer.set("!touch x")`, `zemi.composer.insert("x")`, `zemi.composer.submit()`,
		`zemi.session.new()`, `zemi.session.open("x")`, `zemi.session.rewind("x")`,
		`zemi.memory.add({ kind = "fact", text = "x" })`,
	} {
		if err := r.ExecuteChunk(context.Background(), src, "p.lua", lua.OriginProject); err == nil || !strings.Contains(err.Error(), "only user config") && !strings.Contains(err.Error(), "may not") {
			t.Errorf("%s from project: %v", src, err)
		}
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

// TestLuaToolRunsToolPre: a rewrite is refused rather than running the
// original input the hook meant to replace.
func TestLuaToolRunsToolPre(t *testing.T) {
	for _, c := range []struct{ ret, want string }{
		{`{ decision = "deny", reason = "no" }`, "denied by hook"},
		{`{ decision = "rewrite", input = { city = "x" } }`, "rewrite is not supported"},
	} {
		h, r := newTestHost(t)
		runUser(t, r, `
			zemi.tool.register({ name = "weather", execute = function() return "sunny" end })
			zemi.event.on("tool.pre", function(ev) if ev.tool == "weather" then return `+c.ret+` end end)
		`)
		resp, err := h.luaFantasyTools(h.PipelineFor())[0].Run(context.Background(), fantasy.ToolCall{Input: "{}"})
		if err != nil || !resp.IsError || !strings.Contains(resp.Content, c.want) {
			t.Errorf("%s: resp = %+v, %v", c.ret, resp, err)
		}
	}
}
