// mcp.go bridges MCP servers (DESIGN §13, M5). User config registers them
// with zemi.mcp.add; each starts
// in the background, and every tool it lists becomes a model tool named
// mcp_<server>_<tool>. Calls run tool.pre and the permission gate under
// subject mcp.<server> with the tool name as argument (ask by default).
// Descriptions are labeled with their server: they are third-party text.
package hostapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/mcp"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/tools"
)

// MCPSpec is one server registration.
type MCPSpec struct {
	Command       string
	Args          []string
	Env           map[string]string
	URL           string
	Headers       map[string]string
	EnabledTools  []string // nil = all
	DisabledTools []string
	Timeout       time.Duration // per call; 0 = 2 minutes
	Source        string        // where it was added: init.lua
}

type mcpServer struct {
	name string
	spec MCPSpec

	mu      sync.Mutex
	client  *mcp.Client
	tools   []mcp.Tool
	status  string // starting, ready, failed, stopped
	err     string
	stopped bool
}

type mcpReg struct {
	mu      sync.Mutex
	servers map[string]*mcpServer
}

func newMCPReg() *mcpReg { return &mcpReg{servers: map[string]*mcpServer{}} }

// no "_" in server names: mcp_<server>_<tool> must split one way
var mcpServerName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// addMCP registers (or replaces) a server and starts it in the background.
func (h *Host) addMCP(name string, spec MCPSpec) error {
	if !mcpServerName.MatchString(name) {
		return fmt.Errorf("server name %q must match [a-z0-9][a-z0-9-]{0,31}", name)
	}
	if (spec.Command == "") == (spec.URL == "") {
		return errors.New("give exactly one of command or url")
	}
	s := &mcpServer{name: name, spec: spec, status: "starting"}
	h.mcp.mu.Lock()
	old := h.mcp.servers[name]
	h.mcp.servers[name] = s
	h.mcp.mu.Unlock()
	if old != nil {
		old.stop()
	}
	h.GoSafe("mcp start "+name, func() { h.startMCP(s) })
	return nil
}

func (h *Host) startMCP(s *mcpServer) {
	env, _ := perm.ChildEnv(nil)
	env = append(env, "ZEMI_PROJECT_DIR="+h.Dirs.ProjectDir)
	for k, v := range s.spec.Env {
		env = append(env, k+"="+v)
	}
	// Outside the project even when it is trusted: npx and uv read .npmrc,
	// node_modules/.bin and uv config from the working directory, which
	// sandboxed commands and settled writes may change, and a server runs
	// unconfined because most need the network and home caches a sandbox
	// would refuse. ZEMI_PROJECT_DIR says where the project is.
	headers := map[string]string{}
	maps.Copy(headers, s.spec.Headers)
	cfg := mcp.Config{
		Command: s.spec.Command, Args: s.spec.Args, Env: env, Dir: h.Dirs.DataHome,
		Stderr: serverLogWriter{h: h, source: "mcp", server: s.name},
		URL:    s.spec.URL, Headers: headers,
		OnToolsChanged: func() { h.GoSafe("mcp tools "+s.name, func() { h.refreshMCPTools(s) }) },
	}
	if s.spec.URL != "" && !hasHeader(headers, "Authorization") {
		cfg.Auth = h.mcpAuth(s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// a stopped server was replaced or reloaded away: its failure is no news
	fail := func(err error) {
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			return
		}
		if errors.Is(err, mcp.ErrAuthRequired) {
			s.status, s.err = "needs sign-in", ""
			s.mu.Unlock()
			h.Notify("info", "mcp", s.name+" needs you to sign in: open /mcp")
			h.refresh()
			return
		}
		s.status, s.err = "failed", err.Error()
		s.mu.Unlock()
		h.Log.Warn("mcp", "server failed", map[string]any{"server": s.name, "error": err.Error()})
		h.RecordError("mcp "+s.name, err.Error())
		h.refresh()
	}
	client, err := mcp.Start(ctx, cfg)
	if err != nil {
		fail(err)
		return
	}
	list, err := client.Tools(ctx)
	if err != nil {
		client.Close()
		fail(err)
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		client.Close()
		return
	}
	s.client, s.tools, s.status, s.err = client, list, "ready", ""
	s.mu.Unlock()
	h.Log.Info("mcp", "server ready", map[string]any{"server": s.name, "tools": len(list)})
	h.refresh()
	h.GoSafe("mcp watch "+s.name, func() {
		<-client.Done()
		fail(client.Err())
	})
}

func (h *Host) refreshMCPTools(s *mcpServer) {
	c := s.currentClient()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, err := c.Tools(ctx)
	if err != nil {
		h.Log.Warn("mcp", "tool list refresh failed", map[string]any{"server": s.name, "error": err.Error()})
		return
	}
	s.mu.Lock()
	s.tools = list
	s.mu.Unlock()
}

func (s *mcpServer) currentClient() *mcp.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != "ready" {
		return nil
	}
	return s.client
}

func (s *mcpServer) stop() {
	s.mu.Lock()
	s.stopped, s.status = true, "stopped"
	c := s.client
	s.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

// stopAll stops every server and forgets them (reload replays the
// registrations).
func (r *mcpReg) stopAll() {
	r.mu.Lock()
	servers := r.servers
	r.servers = map[string]*mcpServer{}
	r.mu.Unlock()
	for _, s := range servers {
		s.stop()
	}
}

func (r *mcpReg) list() []*mcpServer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.SortedFunc(maps.Values(r.servers), func(a, b *mcpServer) int { return cmp.Compare(a.name, b.name) })
}

// serverLogWriter logs an MCP or language server's stderr under source.
type serverLogWriter struct {
	h              *Host
	source, server string
}

func (w serverLogWriter) Write(p []byte) (int, error) {
	w.h.Log.Debug(w.source, "stderr: "+truncRunes(strings.TrimSpace(string(p)), 500), map[string]any{"server": w.server})
	return len(p), nil
}

// ---------------------------------------------------------------------------
// tools

var mcpNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func mcpToolName(server, tool string) string {
	n := "mcp_" + server + "_" + mcpNameUnsafe.ReplaceAllString(tool, "_")
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// mcpFantasyTools exposes the ready servers' tools, checked through pipe.
func (h *Host) mcpFantasyTools(pipe *tools.Pipeline) []fantasy.AgentTool {
	var out []fantasy.AgentTool
	seen := map[string]bool{} // "a.b" and "a_b" map to one tool name
	for _, s := range h.mcp.list() {
		s.mu.Lock()
		list, ready := s.tools, s.status == "ready"
		s.mu.Unlock()
		if !ready {
			continue
		}
		for _, t := range list {
			if (s.spec.EnabledTools != nil && !slices.Contains(s.spec.EnabledTools, t.Name)) || slices.Contains(s.spec.DisabledTools, t.Name) {
				continue
			}
			name := mcpToolName(s.name, t.Name)
			if seen[name] {
				h.Log.Warn("mcp", "tool name collides, skipped", map[string]any{"server": s.name, "tool": t.Name})
				continue
			}
			seen[name] = true
			props, err := tools.ResolveSchemaRefs(t.InputSchema)
			if err != nil {
				h.Log.Warn("mcp", "tool schema cannot be flattened, skipped", map[string]any{
					"server": s.name, "tool": t.Name, "error": err.Error(),
				})
				continue
			}
			out = append(out, &mcpTool{h: h, srv: s, tool: t, pipe: pipe, props: props})
		}
	}
	return out
}

type mcpTool struct {
	h     *Host
	srv   *mcpServer
	tool  mcp.Tool
	pipe  *tools.Pipeline
	props map[string]any // input schema properties, local $refs resolved
}

func (t *mcpTool) Info() fantasy.ToolInfo {
	required := []string{}
	if list, ok := t.tool.InputSchema["required"].([]any); ok {
		for _, r := range list {
			if s, ok := r.(string); ok {
				required = append(required, s)
			}
		}
	}
	return fantasy.ToolInfo{
		Name:        mcpToolName(t.srv.name, t.tool.Name),
		Description: "(tool from the " + t.srv.name + " MCP server) " + truncRunes(t.tool.Description, 1024),
		Parameters:  t.props,
		Required:    required,
		Parallel:    true,
	}
}

func (t *mcpTool) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (t *mcpTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (t *mcpTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	h, name := t.h, t.srv.name
	if resp, stop, err := h.preCheck(mcpToolName(name, t.tool.Name), call.Input); stop {
		return resp, err
	}
	var args map[string]any
	if strings.TrimSpace(call.Input) != "" {
		if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
			return fantasy.NewTextErrorResponse("invalid tool input: " + err.Error()), nil
		}
	}
	req := tools.Request{Tool: mcpToolName(name, t.tool.Name), Subject: "mcp." + name, Arg: t.tool.Name, Preview: call.Input}
	if v := t.pipe.Check(req); v.Decision != perm.Allow {
		return fantasy.NewTextErrorResponse("permission denied (" + v.DecidedBy + "): mcp." + name + " " + t.tool.Name), nil
	}
	client := t.srv.currentClient()
	if client == nil {
		return fantasy.NewTextErrorResponse("MCP server " + name + " is not running (/mcp shows why)"), nil
	}
	timeout := t.srv.spec.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := client.CallTool(cctx, t.tool.Name, args)
	if err != nil {
		return fantasy.NewTextErrorResponse("mcp " + name + ": " + err.Error()), nil
	}
	text := truncOutput(res.Text())
	if res.IsError {
		return fantasy.NewTextErrorResponse(text), nil
	}
	if strings.TrimSpace(text) == "" {
		text = "(no output)"
	}
	return fantasy.NewTextResponse(text), nil
}

// ---------------------------------------------------------------------------
// zemi.mcp and /mcp

func (h *Host) mcpNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	// add(name, { command, args, env, url, headers, enabled_tools,
	// disabled_tools, timeout }): user config only, like providers.
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		opts := L.CheckTable(2)
		if h.RT.CurrentOrigin(L) != lua.OriginUser {
			L.RaiseError("zemi.mcp.add: only user config may add MCP servers")
		}
		spec := MCPSpec{
			Command: rawString(opts, "command"), Args: rawStrings(opts, "args"), Env: rawStringMap(opts, "env"),
			URL: rawString(opts, "url"), Headers: rawStringMap(opts, "headers"),
			EnabledTools: rawStrings(opts, "enabled_tools"), DisabledTools: rawStrings(opts, "disabled_tools"),
			Source: "init.lua",
		}
		if n, ok := opts.RawGetString("timeout").(glua.LNumber); ok && n > 0 {
			spec.Timeout = time.Duration(float64(n) * float64(time.Second))
		}
		if err := h.addMCP(name, spec); err != nil {
			L.RaiseError("zemi.mcp.add: %v", err)
		}
		return 0
	}))
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		var rows []any
		for _, s := range h.mcp.list() {
			s.mu.Lock()
			names := make([]string, 0, len(s.tools))
			for _, t := range s.tools {
				names = append(names, t.Name)
			}
			rows = append(rows, map[string]any{"name": s.name, "status": s.status, "error": s.err, "source": s.spec.Source, "tools": names})
			s.mu.Unlock()
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))
	return tb
}

// MCPCommandsStep registers /mcp.
func (h *Host) MCPCommandsStep() lua.BootStep {
	return lua.BootStep{
		Name: "mcp commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			_, err := h.Commands.Add(&Command{
				Name: "mcp", NArgs: "*", Desc: "manage MCP servers",
				GoFn: func(h *Host, args string) error {
					sub, name, _ := strings.Cut(strings.TrimSpace(args), " ")
					switch sub {
					case "":
						return h.openMCP()
					case "restart":
						return h.restartMCP(strings.TrimSpace(name))
					case "sign-in":
						return h.signInMCP(strings.TrimSpace(name))
					case "sign-out":
						return h.signOutMCP(strings.TrimSpace(name))
					}
					return fmt.Errorf("unknown /mcp subcommand %q", sub)
				},
				GoComplete: subcommandCompleter([]string{"restart", "sign-in", "sign-out"}, func(h *Host, _ string) []string {
					var names []string
					for _, s := range h.mcp.list() {
						names = append(names, s.name)
					}
					return names
				}),
				Origin: lua.OriginBuiltin, Gen: Gen(g.ID),
			})
			return err
		},
	}
}

func (h *Host) restartMCP(name string) error {
	s, err := h.mcpServerNamed(name)
	if err != nil {
		return err
	}
	return h.addMCP(name, s.spec)
}

func (h *Host) openMCP() error {
	ui := h.hooks()
	if ui == nil {
		return errors.New("no UI")
	}
	var items []any
	for _, s := range h.mcp.list() {
		s.mu.Lock()
		status, errText := s.status, s.err
		var tools []string
		for _, t := range s.tools {
			tools = append(tools, t.Name)
		}
		s.mu.Unlock()
		desc := status
		if status == "ready" {
			desc = fmt.Sprintf("ready · %d %s", len(tools), plural(len(tools), "tool", "tools"))
		}
		detail := "command: " + strings.Join(append([]string{s.spec.Command}, s.spec.Args...), " ")
		if s.spec.URL != "" {
			detail = "url: " + s.spec.URL
		}
		detail += "\nstatus: " + status
		if errText != "" {
			detail += "\nerror: " + errText
		}
		if len(tools) > 20 {
			tools = append(tools[:20], "...")
		}
		if len(tools) > 0 {
			detail += "\ntools: " + strings.Join(tools, ", ")
		}
		actions := []any{map[string]any{"text": "Restart", "run": "mcp restart " + s.name}}
		if status == "needs sign-in" {
			actions = append(actions, map[string]any{"text": "Sign in", "run": "mcp sign-in " + s.name})
		}
		if _, err := os.Stat(h.mcpAuth(s).Path); s.spec.URL != "" && err == nil {
			actions = append(actions, map[string]any{"text": "Sign out", "run": "mcp sign-out " + s.name})
		}
		items = append(items, map[string]any{"text": s.name, "desc": desc + " · " + s.spec.Source, "detail": detail, "actions": actions})
	}
	ui.OpenOverlay(Overlay{ID: "mcp", Title: "MCP servers", Origin: lua.OriginBuiltin, Items: items,
		Empty: `No MCP servers. Add one in init.lua: zemi.mcp.add("name", { command = "..." })`})
	return nil
}

func hasHeader(headers map[string]string, name string) bool {
	for k := range headers {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// mcpAuth is the stored sign-in of an HTTP server, keyed by name and URL.
func (h *Host) mcpAuth(s *mcpServer) *mcp.Auth {
	return &mcp.Auth{Name: s.name, ServerURL: s.spec.URL, Path: filepath.Join(h.Dirs.DataHome, "mcp-auth", s.name+".json")}
}

func (h *Host) mcpServerNamed(name string) (*mcpServer, error) {
	h.mcp.mu.Lock()
	s := h.mcp.servers[name]
	h.mcp.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("no MCP server %q", name)
	}
	return s, nil
}

// signInMCP runs the browser sign-in in the background and restarts the
// server once it succeeds.
func (h *Host) signInMCP(name string) error {
	s, err := h.mcpServerNamed(name)
	if err != nil {
		return err
	}
	if s.spec.URL == "" {
		return fmt.Errorf("%s runs a local command; only HTTP servers sign in", name)
	}
	auth := h.mcpAuth(s)
	h.GoSafe("mcp sign-in "+name, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err := auth.Authorize(ctx, func(u string) {
			h.Notify("info", "Sign in to "+name, "Finish in your browser. If it did not open, visit "+u)
			h.openBrowser(u)
		})
		if err != nil {
			h.Notify("error", "mcp", "signing in to "+name+" failed: "+err.Error())
			return
		}
		h.Notify("info", "mcp", "signed in to "+name)
		if err := h.restartMCP(name); err != nil {
			h.Notify("error", "mcp", err.Error())
		}
	})
	return nil
}

func (h *Host) signOutMCP(name string) error {
	s, err := h.mcpServerNamed(name)
	if err != nil {
		return err
	}
	if err := h.mcpAuth(s).Logout(); err != nil {
		return err
	}
	h.Notify("info", "mcp", "signed out of "+name)
	return h.restartMCP(name)
}

// openBrowser opens an http(s) URL with the platform's opener; the URL is
// one argument, never shell text.
func (h *Host) openBrowser(u string) {
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return
	}
	argv := map[string][]string{"darwin": {"open"}, "windows": {"rundll32", "url.dll,FileProtocolHandler"}}[runtime.GOOS]
	if argv == nil {
		argv = []string{"xdg-open"}
	}
	cmd := proc.Command(argv[0], append(argv[1:], u)...)
	if proc.Start(cmd) == nil {
		h.GoSafe("open browser", func() { _ = proc.Wait(cmd) })
	}
}
