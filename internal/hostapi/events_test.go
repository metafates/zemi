package hostapi

import (
	"context"
	"strings"
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
	deadline := time.Now().Add(3 * time.Second)
	for {
		if v, _ := h.GlobalDB.GetKV("denied"); v == `"read fs.read rule"` {
			return
		}
		if time.Now().After(deadline) {
			v, _ := h.GlobalDB.GetKV("denied")
			t.Fatalf("tool.denied payload = %s", v)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestLuaAgentsNeedUserConfig: project code cannot start agents or
// workflows from Lua.
func TestLuaAgentsNeedUserConfig(t *testing.T) {
	_, r := newTestHostDB(t)
	for _, src := range []string{`zemi.agent.run({ prompt = "x" })`, `zemi.workflow.run({ script = "return 1" })`} {
		if err := r.ExecuteChunk(context.Background(), src, "p.lua", lua.OriginProject); err == nil || !strings.Contains(err.Error(), "only user config") {
			t.Fatalf("%s from project: %v", src, err)
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
	if _, err := glua.NewState().LoadString(src); err != nil {
		t.Fatalf("review.lua does not compile: %v", err)
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
