// mcp.go bridges MCP servers (DESIGN §13, M5). User config registers them
// with zemi.mcp.add, or crush.json's mcp section imports them; each starts
// in the background, and every tool it lists becomes a model tool named
// mcp_<server>_<tool>. Calls run tool.pre and the permission gate under
// subject mcp.<server> with the tool name as argument (ask by default).
// Descriptions are labeled with their server: they are third-party text.
package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/mcp"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// MCPSpec is one server registration.
type MCPSpec struct {
	Command       string
	Args          []string
	Env           map[string]string // values expand $VARS from zemi's environment
	URL           string
	Headers       map[string]string // values expand $VARS too
	EnabledTools  []string          // nil = all
	DisabledTools []string
	Timeout       time.Duration // per call; 0 = 2 minutes
	Source        string        // init.lua or crush.json
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
	env, _ := jobEnvFilters(nil)
	for k, v := range s.spec.Env {
		env = append(env, k+"="+os.ExpandEnv(v))
	}
	headers := map[string]string{}
	for k, v := range s.spec.Headers {
		headers[k] = os.ExpandEnv(v)
	}
	cfg := mcp.Config{
		Command: s.spec.Command, Args: s.spec.Args, Env: env, Dir: h.Dirs.ProjectDir,
		Stderr: mcpLogWriter{h: h, server: s.name},
		URL:    s.spec.URL, Headers: headers,
	}
	if s.spec.URL != "" && !hasHeader(headers, "Authorization") {
		cfg.Auth = h.mcpAuth(s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fail := func(err error) {
		s.mu.Lock()
		if errors.Is(err, mcp.ErrAuthRequired) {
			s.status, s.err = "needs sign-in", ""
			s.mu.Unlock()
			h.memNotify("info", "mcp", s.name+" needs you to sign in: open /mcp")
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
	client.OnToolsChanged = func() { h.GoSafe("mcp tools "+s.name, func() { h.refreshMCPTools(s) }) }
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
		s.mu.Lock()
		stopped := s.stopped
		s.mu.Unlock()
		if !stopped {
			fail(client.Err())
		}
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
	out := make([]*mcpServer, 0, len(r.servers))
	for _, s := range r.servers {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

type mcpLogWriter struct {
	h      *Host
	server string
}

func (w mcpLogWriter) Write(p []byte) (int, error) {
	w.h.Log.Debug("mcp", "stderr: "+truncRunes(strings.TrimSpace(string(p)), 500), map[string]any{"server": w.server})
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
			if (s.spec.EnabledTools != nil && !contains(s.spec.EnabledTools, t.Name)) || contains(s.spec.DisabledTools, t.Name) {
				continue
			}
			if name := mcpToolName(s.name, t.Name); seen[name] {
				h.Log.Warn("mcp", "tool name collides, skipped", map[string]any{"server": s.name, "tool": t.Name})
				continue
			} else {
				seen[name] = true
			}
			out = append(out, &mcpTool{h: h, srv: s, tool: t, pipe: pipe})
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

type mcpTool struct {
	h    *Host
	srv  *mcpServer
	tool mcp.Tool
	pipe *tools.Pipeline
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
		Parameters:  tools.SchemaProperties(t.tool.InputSchema),
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
	req := tools.Request{Tool: mcpToolName(name, t.tool.Name), Subject: "mcp." + name, Arg: t.tool.Name, Reason: "call with " + truncRunes(call.Input, 300)}
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
// zemi.mcp, /mcp and the crush.json import

func (h *Host) mcpNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	// add(name, { command, args, env, url, headers, enabled_tools,
	// disabled_tools, timeout }): user config only, like providers.
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		opts := L.CheckTable(2)
		if h.RT.CurrentOrigin() != lua.OriginUser {
			L.RaiseError("zemi.mcp.add: only user config may add MCP servers")
			return 0
		}
		strs := func(key string) []string {
			list, ok := opts.RawGetString(key).(*glua.LTable)
			if !ok {
				return nil
			}
			out := []string{}
			list.ForEach(func(_, v glua.LValue) {
				if s, ok := v.(glua.LString); ok {
					out = append(out, string(s))
				}
			})
			return out
		}
		strMap := func(key string) map[string]string {
			out := map[string]string{}
			if m, ok := opts.RawGetString(key).(*glua.LTable); ok {
				m.ForEach(func(k, v glua.LValue) {
					if ks, ok := k.(glua.LString); ok {
						out[string(ks)] = v.String()
					}
				})
			}
			return out
		}
		spec := MCPSpec{
			Command: rawString(opts, "command"), Args: strs("args"), Env: strMap("env"),
			URL: rawString(opts, "url"), Headers: strMap("headers"),
			EnabledTools: strs("enabled_tools"), DisabledTools: strs("disabled_tools"),
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
		out := L.NewTable()
		for _, s := range h.mcp.list() {
			s.mu.Lock()
			row := L.NewTable()
			row.RawSetString("name", glua.LString(s.name))
			row.RawSetString("status", glua.LString(s.status))
			row.RawSetString("error", glua.LString(s.err))
			row.RawSetString("source", glua.LString(s.spec.Source))
			names := L.NewTable()
			for _, t := range s.tools {
				names.Append(glua.LString(t.Name))
			}
			row.RawSetString("tools", names)
			s.mu.Unlock()
			out.Append(row)
		}
		L.Push(out)
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
				Name: "mcp", NArgs: "*", Desc: "MCP servers and their status; restart <name>",
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
	for _, s := range h.mcp.list() {
		if s.name == name {
			return h.addMCP(name, s.spec)
		}
	}
	return fmt.Errorf("no MCP server %q", name)
}

func (h *Host) openMCP() error {
	ui := h.hooks()
	if ui == nil {
		return errors.New("no UI")
	}
	servers := h.mcp.list()
	if len(servers) == 0 {
		return errors.New(`no MCP servers: add one in init.lua with zemi.mcp.add("name", { command = "...", args = { ... } })`)
	}
	items := make([]any, 0, len(servers))
	for _, s := range servers {
		s.mu.Lock()
		state, run := fmt.Sprintf("%d tools", len(s.tools)), "mcp restart "+s.name
		if s.status != "ready" {
			state = s.status
		}
		if s.status == "needs sign-in" {
			state, run = "needs sign-in, enter opens your browser", "mcp sign-in "+s.name
		}
		if s.err != "" {
			state += ": " + truncRunes(s.err, 60)
		}
		s.mu.Unlock()
		items = append(items, map[string]any{
			"text": fmt.Sprintf("%-16s %-10s %s", s.name, s.spec.Source, state),
			"run":  run,
		})
	}
	ui.OpenOverlay(Overlay{ID: "mcp", Title: "MCP servers (enter restarts or signs in)", Origin: lua.OriginBuiltin, Items: items})
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
	for _, s := range h.mcp.list() {
		if s.name == name {
			return s, nil
		}
	}
	return nil, fmt.Errorf("no MCP server %q", name)
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
			h.memNotify("info", "Sign in to "+name, "Finish in your browser. If it did not open, visit "+u)
			h.openBrowser(u)
		})
		if err != nil {
			h.memNotify("error", "mcp", "signing in to "+name+" failed: "+err.Error())
			return
		}
		h.memNotify("info", "mcp", "signed in to "+name)
		if err := h.restartMCP(name); err != nil {
			h.memNotify("error", "mcp", err.Error())
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
	h.memNotify("info", "mcp", "signed out of "+name)
	return h.restartMCP(name)
}

// openBrowser opens an http(s) URL with the platform's opener; the URL is
// one argument, never shell text.
func (h *Host) openBrowser(u string) {
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if cmd.Start() == nil {
		h.GoSafe("open browser", func() { _ = cmd.Wait() })
	}
}

// ImportCrushMCP registers the enabled servers from crush.json's mcp
// section that init.lua did not already define. Problems are logged, never
// fatal.
func (h *Host) ImportCrushMCP(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var file struct {
		MCP map[string]struct {
			Type          string            `json:"type"`
			Command       string            `json:"command"`
			Args          []string          `json:"args"`
			Env           map[string]string `json:"env"`
			URL           string            `json:"url"`
			Headers       map[string]string `json:"headers"`
			Disabled      bool              `json:"disabled"`
			EnabledTools  []string          `json:"enabled_tools"`
			DisabledTools []string          `json:"disabled_tools"`
			Timeout       int               `json:"timeout"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		h.Log.Warn("mcp", "crush.json mcp import failed", map[string]any{"error": err.Error()})
		return
	}
	existing := map[string]bool{}
	for _, s := range h.mcp.list() {
		existing[s.name] = true
	}
	for raw, c := range file.MCP {
		name := strings.ToLower(strings.ReplaceAll(mcpNameUnsafe.ReplaceAllString(raw, "-"), "_", "-"))
		if c.Disabled || existing[name] {
			continue
		}
		if c.Type == "sse" {
			h.Log.Warn("mcp", "crush.json server uses the old SSE transport, skipped", map[string]any{"server": raw})
			continue
		}
		spec := MCPSpec{Command: c.Command, Args: c.Args, Env: c.Env, URL: c.URL, Headers: c.Headers,
			EnabledTools: c.EnabledTools, DisabledTools: c.DisabledTools, Source: "crush.json"}
		if c.Timeout > 0 {
			spec.Timeout = time.Duration(c.Timeout) * time.Second
		}
		if err := h.addMCP(name, spec); err != nil {
			h.Log.Warn("mcp", "crush.json server skipped", map[string]any{"server": raw, "error": err.Error()})
		}
	}
}
