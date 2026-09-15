package hostapi

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/mcp/mcptest"
	"github.com/metafates/zemi/internal/perm"
)

// TestMCPBridge registers a fake server from Lua and drives its tool the
// way the model does: filtered by disabled_tools, asking (and failing
// closed without a UI) until a rule allows it.
func TestMCPBridge(t *testing.T) {
	srv := httptest.NewServer(mcptest.Handler())
	defer srv.Close()
	h, r := newTestHostDB(t)
	defer h.mcp.stopAll()
	runUser(t, r, fmt.Sprintf(`zemi.mcp.add("fake", { url = %q, disabled_tools = { "fail" } })`, srv.URL))
	if err := r.ExecuteChunk(context.Background(), `zemi.mcp.add("evil", { command = "sh" })`, "p.lua", lua.OriginProject); err == nil {
		t.Fatal("project config must not add MCP servers")
	}

	var tools []fantasy.AgentTool
	for deadline := time.Now().Add(3 * time.Second); len(tools) == 0; {
		if time.Now().After(deadline) {
			t.Fatalf("server never became ready: %+v", h.mcp.list())
		}
		time.Sleep(10 * time.Millisecond)
		tools = h.mcpFantasyTools(h.PipelineFor())
	}
	if len(tools) != 1 || tools[0].Info().Name != "mcp_fake_echo" {
		t.Fatalf("tools = %v", tools)
	}
	run := func() fantasy.ToolResponse {
		resp, err := tools[0].Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "mcp_fake_echo", Input: `{"text":"hi"}`})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := run(); !resp.IsError {
		t.Fatalf("unruled call must fail closed: %+v", resp)
	}
	h.Perm.Add(perm.Rule{Subject: "mcp.fake", Pattern: "echo", Decision: perm.Allow})
	if resp := run(); resp.IsError || resp.Content != "hi" {
		t.Fatalf("allowed call = %+v", resp)
	}
}
