package hostapi

import (
	"context"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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

// toolsModel records the MCP tools and tool_search each request offers.
type toolsModel struct {
	*scriptModel
	mu   sync.Mutex
	sent []string
}

func (m *toolsModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if sideCall(call) {
		return m.scriptModel.Stream(ctx, call)
	}
	var names []string
	for _, tl := range call.Tools {
		if n := tl.GetName(); strings.HasPrefix(n, mcpToolPrefix) || n == tools.ToolSearch {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	m.mu.Lock()
	m.sent = append(m.sent, strings.Join(names, " "))
	m.mu.Unlock()
	return m.scriptModel.Stream(ctx, call)
}

// TestMCPDeferred: past ten MCP tools a step offers only tool_search and
// the tools a search or the resumed history loaded. A send "search:<q>" or
// "call:<tool>" makes the model call that tool once; want is what each
// request offered.
func TestMCPDeferred(t *testing.T) {
	cases := []struct {
		name    string
		servers int
		sends   []string // "new" and "resume" switch sessions instead
		want    []string
	}{
		{"ten or fewer ride every step", 3, []string{"hi"}, []string{
			"mcp_a_echo mcp_a_fail mcp_a_shot mcp_b_echo mcp_b_fail mcp_b_shot mcp_c_echo mcp_c_fail mcp_c_shot"}},
		{"past ten only loaded tools are offered", 4, []string{"search:echo"}, []string{
			"tool_search",
			"mcp_a_echo mcp_b_echo mcp_c_echo mcp_d_echo tool_search"}},
		{"an exact name wins over more keyword hits", 4, []string{"search:text the screenshot mcp_d_fail"}, []string{
			"tool_search",
			"mcp_a_echo mcp_a_shot mcp_b_echo mcp_b_shot mcp_d_fail tool_search"}},
		{"new drops them and resume reseeds the called ones", 4, []string{"search:shot", "call:mcp_c_shot", "new", "hi", "resume", "hi"}, []string{
			"tool_search", "mcp_a_shot mcp_b_shot mcp_c_shot mcp_d_shot tool_search",
			"mcp_a_shot mcp_b_shot mcp_c_shot mcp_d_shot tool_search", "mcp_a_shot mcp_b_shot mcp_c_shot mcp_d_shot tool_search",
			"tool_search",
			"mcp_c_shot tool_search"}},
	}
	srv := httptest.NewServer(mcptest.Handler())
	defer srv.Close()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, r := newTestHostDB(t)
			defer h.mcp.stopAll()
			h.reflectMu.Lock() // a session close must not start a reflect run
			defer h.reflectMu.Unlock()
			for _, name := range []string{"a", "b", "c", "d"}[:tc.servers] {
				runUser(t, r, fmt.Sprintf(`zemi.mcp.add(%q, { url = %q })`, name, srv.URL))
			}
			waitFor(t, func() error {
				if n := len(h.mcpFantasyTools(h.PipelineFor())); n != 3*tc.servers {
					return fmt.Errorf("%d MCP tools ready", n)
				}
				return nil
			})
			m := &toolsModel{scriptModel: &scriptModel{script: func(_, last string, toolResult bool) []fantasy.StreamPart {
				if q, ok := strings.CutPrefix(last, "search:"); ok && !toolResult {
					return toolCallParts("s1", tools.ToolSearch, fmt.Sprintf(`{"query":%q}`, q))
				}
				if name, ok := strings.CutPrefix(last, "call:"); ok && !toolResult {
					return toolCallParts("c1", name, `{"text":"x"}`)
				}
				return textParts("ok")
			}}}
			h.modelFn = func() (fantasy.LanguageModel, error) { return m, nil }
			first := ""
			for _, send := range tc.sends {
				var err error
				switch send {
				case "new":
					first = h.AgentSessionID()
					err = h.NewSession()
				case "resume":
					err = h.ResumeSession(first)
				default:
					if err = h.AgentSend(send); err == nil {
						waitIdle(t, h)
					}
				}
				if err != nil {
					t.Fatalf("%s: %v", send, err)
				}
			}
			if !slices.Equal(m.sent, tc.want) {
				t.Fatalf("requests offered\n%q\nwant\n%q", m.sent, tc.want)
			}
		})
	}
}
