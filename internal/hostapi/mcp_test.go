package hostapi

import (
	"context"
	"fmt"
	"net/http/httptest"
	"slices"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/mcp/mcptest"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
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

	var list []fantasy.AgentTool
	waitFor(t, func() error {
		if list = h.mcpFantasyTools(h.PipelineFor()); len(list) == 0 {
			return fmt.Errorf("server never became ready: %+v", h.mcp.list())
		}
		return nil
	})
	names := []string{}
	for _, tl := range list {
		names = append(names, tl.Info().Name)
	}
	if !slices.Equal(names, []string{"mcp_fake_echo", "mcp_fake_shot"}) {
		t.Fatalf("tools = %v", names)
	}
	run := func(i int, input string) fantasy.ToolResponse {
		resp, err := list[i].Run(context.Background(), fantasy.ToolCall{ID: "1", Name: names[i], Input: input})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := run(0, `{"text":"hi"}`); !resp.IsError {
		t.Fatalf("unruled call must fail closed: %+v", resp)
	}
	h.Perm.Add(perm.Rule{Subject: "mcp.fake", Pattern: "*", Decision: perm.Allow})
	if resp := run(0, `{"text":"hi"}`); resp.IsError || resp.Content != "hi" {
		t.Fatalf("allowed call = %+v", resp)
	}

	// the server marks shot read-only, so auto mode settles it without
	// asking, and its png reaches the model instead of a placeholder
	if !h.autoLocal(tools.Request{Subject: "mcp.fake", Arg: "shot"}) {
		t.Error("a read-only MCP tool did not settle in auto mode")
	}
	if h.autoLocal(tools.Request{Subject: "mcp.fake", Arg: "echo"}) {
		t.Error("an unannotated MCP tool settled locally")
	}
	resp := run(1, `{"text":"x"}`)
	if resp.Type != "image" || resp.MediaType != "image/png" || len(resp.Data) == 0 || resp.Content != "the screen\n[image image/png]" {
		t.Fatalf("screenshot = %+v", resp)
	}
}

// TestMCPStoppedStartSilent: a server replaced or reloaded while it starts
// keeps its stopped status and records no error when its start fails.
func TestMCPStoppedStartSilent(t *testing.T) {
	h, _ := newTestHost(t)
	s := &mcpServer{name: "gone", spec: MCPSpec{Command: "/nonexistent/mcp"}, status: "stopped", stopped: true}
	h.startMCP(s)
	if s.status != "stopped" || len(h.errs.entries) != 0 {
		t.Fatalf("status %q, errors %v", s.status, h.errs.entries)
	}
}
