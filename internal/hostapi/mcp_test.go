package hostapi

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/mcp/mcptest"
	"github.com/metafates/zemi/internal/tools"
)

// TestLuaToolDrivesDeferredMCP: an MCP tool whose schema a step is holding
// back is one script call away, zemi.tool.list() names it, and the script
// gets its whole output where a direct call is cut.
func TestLuaToolDrivesDeferredMCP(t *testing.T) {
	h, r := newTestHostDB(t)
	defer h.mcp.stopParked()
	defer h.mcp.park()
	h.SetProjectTrusted(true)
	ui := &fakeUI{askAllowed: true, askBy: "user"}
	h.SetUIHooks(ui)
	srv := httptest.NewServer(mcptest.Handler())
	defer srv.Close()
	for _, name := range []string{"a", "b", "c", "d"} {
		runUser(t, r, fmt.Sprintf(`zemi.mcp.add(%q, { url = %q })`, name, srv.URL))
	}
	waitFor(t, func() error {
		if n := len(h.mcpFantasyTools(h.PipelineFor())); n != 12 {
			return fmt.Errorf("%d of 12 MCP tools are up: %+v", n, h.mcp.list())
		}
		return nil
	})
	// past ten MCP tools a step offers only tool_search and what a search
	// loaded, so a script's own list is where these names come from
	_, active := h.deferMCP(h.allTools(h.PipelineFor()), new(sync.Map))
	if active == nil || slices.Contains(active(), "mcp_a_echo") {
		t.Fatalf("the step offers the MCP tools anyway: %v", active())
	}
	var tool fantasy.AgentTool
	set, err := h.profileTools(nil)
	if err != nil {
		t.Fatal(err)
	}
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
	out := run(`
local names = {}
for _, t in ipairs(zemi.tool.list()) do
	if t.mcp then names[#names + 1] = t.name end
end
table.sort(names)
local text, err = zemi.tool.call("mcp_a_echo", { text = "hi" }):wait()
print(err or text)
print(#names, names[1], names[#names])
`)
	if !strings.Contains(out, "hi\n") || !strings.Contains(out, "12\tmcp_a_echo\tmcp_d_shot") {
		t.Fatalf("the script reached %q", out)
	}
	// the whole output, not the cut a direct call gets
	big := strings.Repeat("x", 40000)
	if out := run(`local o = zemi.tool.call("mcp_a_echo", { text = "` + big + `" }):wait() print(#o)`); strings.TrimSpace(out) != "40000" {
		t.Fatalf("a script's MCP call came back %q", strings.TrimSpace(out))
	}
	if len(ui.asked) != 0 {
		t.Fatalf("a call the rule allows asked: %+v", ui.asked)
	}
}

// TestMCPBridge registers a fake server from Lua and drives its tool the
// way the model does: filtered by enabled_tools and disabled_tools, gated by
// the permission mode, through a reload, a restart and an expired sign-in.
func TestMCPBridge(t *testing.T) {
	var expired atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if expired.Load() {
			http.Error(w, "sign in", http.StatusUnauthorized)
			return
		}
		mcptest.Handler().ServeHTTP(w, r)
	}))
	defer srv.Close()
	h, r := newTestHostDB(t)
	defer h.mcp.stopParked()
	defer h.mcp.park()
	runUser(t, r, fmt.Sprintf(`zemi.mcp.add("fake", { url = %q, enabled_tools = { "echo", "shot", "fail", "serach" }, disabled_tools = { "fail" } })`, srv.URL))
	if err := r.ExecuteChunk(t.Context(), `zemi.mcp.add("evil", { command = "sh" })`, "p.lua", lua.OriginProject); err == nil {
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
	if !slices.ContainsFunc(h.errs.entries, func(e errEntry) bool { return strings.Contains(e.msg, "serach") }) {
		t.Errorf("the enabled_tools typo was not reported: %v", h.errs.entries)
	}
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	row := func() map[string]any { return ui.overlays()[0].Items[0].(map[string]any) }
	if err := h.openMCP(""); err != nil || row()["desc"] != "ready · 2 of 3 tools" || row()["mark"] != "ok" || ui.overlays()[0].Reload == nil {
		t.Errorf("/mcp row = %v, %v", ui.overlays(), err)
	}
	run := func(i int, input string) fantasy.ToolResponse {
		resp, err := list[i].Run(t.Context(), fantasy.ToolCall{ID: "1", Name: names[i], Input: input})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	// in ask mode the user's no refuses the call; allow mode runs it
	if err := h.SetPermMode("ask"); err != nil {
		t.Fatal(err)
	}
	if resp := run(0, `{"text":"hi"}`); !resp.IsError {
		t.Fatalf("a call nobody can answer must fail closed: %+v", resp)
	}
	if err := h.SetPermMode("allow"); err != nil {
		t.Fatal(err)
	}
	if resp := run(0, `{"text":"hi"}`); resp.IsError || resp.Content != "hi" {
		t.Fatalf("allowed call = %+v", resp)
	}
	resp := run(1, `{"text":"x"}`)
	if resp.Type != "image" || resp.MediaType != "image/png" || len(resp.Data) == 0 || resp.Content != "the screen\n[image image/png]" {
		t.Fatalf("screenshot = %+v", resp)
	}

	first, _ := h.mcpServerNamed("fake")
	readd := lua.BootStep{Name: "mcp", Run: func(context.Context, *lua.Runtime, *lua.Generation) error { return h.addMCP("fake", first.spec) }}
	if err := h.ForceReload(t.Context(), []lua.BootStep{h.NamespacesStep(), readd}); err != nil {
		t.Fatal(err)
	}
	if s, _ := h.mcpServerNamed("fake"); s != first || s.currentClient() == nil {
		t.Fatal("a reload with the same spec restarted the server")
	}
	// a tool from the snapshot before the restart calls the new server
	if err := h.restartMCP("fake"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if s, _ := h.mcpServerNamed("fake"); s == first || s.currentClient() == nil {
			return errors.New("the restarted server never became ready")
		}
		return nil
	})
	if resp := run(0, `{"text":"hi"}`); resp.IsError {
		t.Fatalf("call after a restart = %+v", resp)
	}
	expired.Store(true)
	if resp := run(0, `{"text":"hi"}`); !resp.IsError || !strings.Contains(resp.Content, "sign in again") {
		t.Fatalf("call after the sign-in expired = %+v", resp)
	}
	if s, _ := h.mcpServerNamed("fake"); s.currentClient() != nil || s.status != StatusSignIn {
		t.Fatalf("status after the sign-in expired = %q, %q", s.status, s.err)
	}
	if items, _ := ui.overlays()[0].Reload(); !strings.Contains(fmt.Sprint(items[0].(map[string]any)["actions"]), "hotkey:s run:mcp sign-in fake") {
		t.Errorf("the rebuilt /mcp row offers no [s] Sign in: %v", items)
	}
	// a reload that disables echo stops the snapshot's echo too
	expired.Store(false)
	spec := first.spec
	spec.DisabledTools = []string{"echo"}
	readd.Run = func(context.Context, *lua.Runtime, *lua.Generation) error { return h.addMCP("fake", spec) }
	if err := h.ForceReload(t.Context(), []lua.BootStep{h.NamespacesStep(), readd}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if s, _ := h.mcpServerNamed("fake"); s.currentClient() == nil {
			return errors.New("the reloaded server never became ready")
		}
		return nil
	})
	if resp := run(0, `{"text":"hi"}`); !resp.IsError || !strings.Contains(resp.Content, "no longer offers echo") {
		t.Fatalf("disabled tool call = %+v", resp)
	}
	// a sign-out waits for the next sign-in at once, since a start could
	// only fail the same way, and redraws the open /mcp
	n := len(ui.overlays())
	signed, _ := h.mcpServerNamed("fake")
	grant := h.mcpAuth(signed).Path
	if err := os.MkdirAll(filepath.Dir(grant), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grant, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.signOutMCP("fake", true); err != nil {
		t.Fatal(err)
	}
	out, _ := h.mcpServerNamed("fake")
	if out.status != StatusSignIn {
		t.Errorf("status after a sign-out = %q, want no start", out.status)
	}
	if err := h.ForceReload(t.Context(), []lua.BootStep{h.NamespacesStep(), readd}); err != nil {
		t.Fatal(err)
	}
	if s, _ := h.mcpServerNamed("fake"); s != out || s.status != StatusSignIn {
		t.Errorf("a reload after a sign-out started the server: %q", s.status)
	}
	if !slices.ContainsFunc(ui.overlays()[n:], func(o Overlay) bool {
		row := o.Items[0].(map[string]any)
		return o.Refresh && row["desc"] == "needs sign-in" && strings.Contains(fmt.Sprint(row["actions"]), "run:mcp sign-in fake")
	}) {
		t.Errorf("no /mcp refresh shows the sign-out: %+v", ui.overlays()[n:])
	}
	// the grant is gone now, so a second sign-out has nothing to park
	if err := h.signOutMCP("fake", true); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("sign-out with no grant = %v, want a refusal", err)
	}
	// waiting in the browser: the row says so and y copies the URL
	out.mu.Lock()
	out.signIn = "https://auth.example.com/authorize?client_id=fake"
	out.mu.Unlock()
	if row := h.mcpItems()[0].(map[string]any); row["desc"] != "waiting in the browser" || row["mark"] != "wait" ||
		row["copy"] != "https://auth.example.com/authorize?client_id=fake" {
		t.Errorf("a waiting row = %v", row)
	}
}

// TestMCPSignInAll: the batch queue signs each waiting HTTP server in on its
// own and ends in one summary toast; a refusal fails its server alone and
// never starts one that did not ask.
func TestMCPSignInAll(t *testing.T) {
	good, bad := &mcptest.Auth{}, &mcptest.Auth{}
	goodSrv := httptest.NewServer(good.Handler())
	defer goodSrv.Close()
	good.Base.Store(goodSrv.URL)
	badSrv := httptest.NewServer(bad.Handler())
	defer badSrv.Close()
	bad.Base.Store(badSrv.URL)
	bad.Refuse.Store(true)
	h, r := newTestHostDB(t)
	defer h.mcp.stopParked()
	defer h.mcp.park()
	if err := h.signInAllMCP(); err == nil || !strings.Contains(err.Error(), "no MCP server is waiting") {
		t.Fatalf("an empty queue = %v, want a refusal", err)
	}
	h.SetUIHooks(&fakeUI{})
	// the test is the browser: the queue's open follows the authorization
	// redirect back to zemi's callback
	h.browser = func(u string) {
		go func() {
			resp, err := http.Get(u)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	runUser(t, r, fmt.Sprintf(`zemi.mcp.add("good", { url = %q })`, goodSrv.URL+"/mcp"))
	runUser(t, r, fmt.Sprintf(`zemi.mcp.add("bad", { url = %q })`, badSrv.URL+"/mcp"))
	waitFor(t, func() error {
		for _, name := range []string{"good", "bad"} {
			if s, _ := h.mcpServerNamed(name); s.status != StatusSignIn {
				return fmt.Errorf("%s = %q, want a sign-in request", name, s.status)
			}
		}
		return nil
	})
	o := h.mcpOverlay(false)
	if len(o.Keys) != 1 || o.Keys[0].Key != "a" || o.Keys[0].Run != "mcp sign-in-all" {
		t.Fatalf("two waiting servers offer no batch key: %+v", o.Keys)
	}
	if err := h.signInAllMCP(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		s, _ := h.mcpServerNamed("good")
		if s.status != StatusReady {
			return fmt.Errorf("good = %q", s.status)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.signIn != "" {
			return errors.New("good still waits in the browser")
		}
		return nil
	})
	if s, _ := h.mcpServerNamed("bad"); s.status != StatusSignIn {
		t.Fatalf("bad = %q, want it still waiting", s.status)
	}
	if o := h.mcpOverlay(false); len(o.Keys) != 0 {
		t.Errorf("one waiting server still offers the batch key: %+v", o.Keys)
	}
	waitFor(t, func() error {
		h.evlog.mu.Lock()
		defer h.evlog.mu.Unlock()
		for _, n := range h.evlog.notes {
			if n.title == "Signed in to 1 of 2" && n.body == "bad failed" {
				return nil
			}
		}
		return errors.New("no summary toast")
	})
}

// TestMCPMedia: an image goes out the size the read tool's would, and one
// no provider takes stays out with the reason in the text.
func TestMCPMedia(t *testing.T) {
	var wide bytes.Buffer
	if err := png.Encode(&wide, image.NewRGBA(image.Rect(0, 0, 3000, 1000))); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, mime string
		data       []byte
		want       string
	}{
		{"a big png is shrunk", "image/png", wide.Bytes(), "image/png 2000x666"},
		{"an svg is refused with a reason", "image/svg+xml", []byte("<svg/>"), "x\n(the image is not shown: not a readable image: image: unknown format)"},
	} {
		resp := mcpMedia("x", c.data, c.mime, nil)
		got := resp.Content
		if resp.Type == "image" {
			cfg, _, _ := image.DecodeConfig(bytes.NewReader(resp.Data))
			got = fmt.Sprintf("%s %dx%d", resp.MediaType, cfg.Width, cfg.Height)
		}
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// TestMCPStoppedStartSilent: a server replaced or reloaded while it starts
// keeps its stopped status and records no error when its start fails.
func TestMCPStoppedStartSilent(t *testing.T) {
	h, _ := newTestHost(t)
	s := &mcpServer{name: "gone", spec: mcpSpec{Command: "/nonexistent/mcp"}, status: "stopped", stopped: true}
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
			"mcp_a_echo mcp_a_fail mcp_a_shot mcp_b_echo mcp_b_fail mcp_b_shot mcp_c_echo mcp_c_fail mcp_c_shot",
		}},
		{"past ten only loaded tools are offered", 4, []string{"search:echo"}, []string{
			"tool_search",
			"mcp_a_echo mcp_b_echo mcp_c_echo mcp_d_echo tool_search",
		}},
		{"an exact name wins over more keyword hits", 4, []string{"search:text the screenshot mcp_d_fail"}, []string{
			"tool_search",
			"mcp_a_echo mcp_a_shot mcp_b_echo mcp_b_shot mcp_d_fail tool_search",
		}},
		{"a repeated search loads the next matches", 4, []string{"search:text", "search:text"}, []string{
			"tool_search",
			"mcp_a_echo mcp_a_fail mcp_a_shot mcp_b_echo mcp_b_shot tool_search",
			"mcp_a_echo mcp_a_fail mcp_a_shot mcp_b_echo mcp_b_shot tool_search",
			"mcp_a_echo mcp_a_fail mcp_a_shot mcp_b_echo mcp_b_fail mcp_b_shot mcp_c_echo mcp_c_fail mcp_c_shot mcp_d_echo tool_search",
		}},
		{"new drops them and resume reseeds the called ones", 4, []string{"search:shot", "call:mcp_c_shot", "new", "hi", "resume", "hi"}, []string{
			"tool_search", "mcp_a_shot mcp_b_shot mcp_c_shot mcp_d_shot tool_search",
			"mcp_a_shot mcp_b_shot mcp_c_shot mcp_d_shot tool_search", "mcp_a_shot mcp_b_shot mcp_c_shot mcp_d_shot tool_search",
			"tool_search",
			"mcp_c_shot tool_search",
		}},
	}
	srv := httptest.NewServer(mcptest.Handler())
	defer srv.Close()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, r := newTestHostDB(t)
			defer h.mcp.stopParked()
			defer h.mcp.park()
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
