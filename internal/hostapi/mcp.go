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
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
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
	ClientID      string // OAuth client registered with the server by hand
	ClientSecret  string
	CallbackPort  int      // fixed OAuth redirect port; 0 = any of zemi's
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
	parked  map[string]*mcpServer // the replaced generation's, until its replay is done
}

func newMCPReg() *mcpReg {
	return &mcpReg{servers: map[string]*mcpServer{}, parked: map[string]*mcpServer{}}
}

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
	old, parked := h.mcp.servers[name], h.mcp.parked[name]
	delete(h.mcp.parked, name)
	// a reload that adds a live server again keeps it: a restart would lose
	// its state (a browser, a connection) and move the tool list
	if old == nil && parked != nil && parked.keeps(spec) {
		h.mcp.servers[name] = parked
		h.mcp.mu.Unlock()
		return nil
	}
	h.mcp.servers[name] = s
	h.mcp.mu.Unlock()
	for _, o := range []*mcpServer{old, parked} {
		if o != nil {
			o.stop()
		}
	}
	h.GoSafe("mcp start "+name, func() { h.startMCP(s) })
	return nil
}

// keeps reports whether s can serve spec as it is: a failed server restarts.
func (s *mcpServer) keeps(spec MCPSpec) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return (s.status == "ready" || s.status == "starting") && reflect.DeepEqual(s.spec, spec)
}

func (h *Host) startMCP(s *mcpServer) {
	env, _ := h.Perm.ChildEnv(nil)
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
		if errors.Is(err, mcp.ErrAuthRequired) {
			s.needsSignIn(h)
			return
		}
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
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
	h.checkToolNames(s, list)
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
	h.checkToolNames(s, list)
}

// checkToolNames records the enabled_tools and disabled_tools entries the
// server lists no tool for: a typo would hide or keep a tool silently.
func (h *Host) checkToolNames(s *mcpServer, list []mcp.Tool) {
	var unknown []string
	for _, n := range slices.Concat(s.spec.EnabledTools, s.spec.DisabledTools) {
		if !slices.ContainsFunc(list, func(t mcp.Tool) bool { return t.Name == n }) {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		h.RecordErrorOnce("mcp "+s.name, "enabled_tools or disabled_tools names no tool the server lists: "+strings.Join(unknown, ", "))
	}
}

// needsSignIn marks s as waiting for the user to sign in, unless it was
// stopped (replaced or reloaded away) or already waits.
func (s *mcpServer) needsSignIn(h *Host) {
	s.mu.Lock()
	if s.stopped || s.status == "needs sign-in" {
		s.mu.Unlock()
		return
	}
	s.status, s.err = "needs sign-in", ""
	s.mu.Unlock()
	h.Notify("info", "MCP", s.name+" needs you to sign in: open /mcp")
	h.refresh()
}

// offered reports whether enabled_tools and disabled_tools give the model
// the tool.
func (s *mcpServer) offered(name string) bool {
	return (s.spec.EnabledTools == nil || slices.Contains(s.spec.EnabledTools, name)) && !slices.Contains(s.spec.DisabledTools, name)
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

// park sets the servers aside for a reload: addMCP takes back each one the
// replay adds with the same spec, and stopParked stops the rest.
func (r *mcpReg) park() {
	r.mu.Lock()
	maps.Copy(r.parked, r.servers)
	r.servers = map[string]*mcpServer{}
	r.mu.Unlock()
}

func (r *mcpReg) stopParked() {
	r.mu.Lock()
	parked := r.parked
	r.parked = map[string]*mcpServer{}
	r.mu.Unlock()
	for _, s := range parked {
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

// mcpToolPrefix marks a tool as an MCP server's, for the model and for
// /usage's prompt breakdown.
const mcpToolPrefix = "mcp_"

func mcpToolName(server, tool string) string {
	n := mcpToolPrefix + server + "_" + mcpNameUnsafe.ReplaceAllString(tool, "_")
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// mcpFantasyTools exposes the ready servers' tools, checked through pipe.
// mcpReadOnly reports a tool the server marked read-only and not
// destructive. It is the server's own word, which is the trust the user
// already extends by adding the server (zemi.mcp.add is user config only).
func (h *Host) mcpReadOnly(server, tool string) bool {
	for _, s := range h.mcp.list() {
		if s.name != server {
			continue
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, t := range s.tools {
			if t.Name == tool {
				return t.ReadOnly()
			}
		}
	}
	return false
}

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
			if !s.offered(t.Name) {
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

// mcpDeferAt is the most MCP tools whose schemas ride every step: past it
// only the ones tool_search loaded are sent, since a big catalog costs
// every request and is rarely called.
const mcpDeferAt = 10

// mcpSearchMax is the most tools one search loads.
const mcpSearchMax = 5

// deferMCP adds tool_search to a set holding more than mcpDeferAt MCP tools
// and returns the names each step offers: every other tool, tool_search and
// the MCP tools in loaded. A nil func offers every tool.
func (h *Host) deferMCP(set []fantasy.AgentTool, loaded *sync.Map) ([]fantasy.AgentTool, func() []string) {
	var deferred []*mcpTool
	var always []string
	for _, t := range set {
		if m, ok := t.(*mcpTool); ok {
			deferred = append(deferred, m)
		} else {
			always = append(always, t.Info().Name)
		}
	}
	if len(deferred) <= mcpDeferAt {
		return set, nil
	}
	always = append(always, tools.ToolSearch)
	active := func() []string {
		out := slices.Clone(always)
		loaded.Range(func(k, _ any) bool { out = append(out, k.(string)); return true })
		return out
	}
	return append(set, newToolSearch(h, deferred, loaded)), active
}

// seedLoaded marks the MCP tools a history called as loaded, so a resumed
// conversation keeps their schemas.
func seedLoaded(loaded *sync.Map, history []fantasy.Message) {
	for _, m := range history {
		for _, part := range m.Content {
			if call, ok := part.(fantasy.ToolCallPart); ok && strings.HasPrefix(call.ToolName, mcpToolPrefix) {
				loaded.Store(call.ToolName, true)
			}
		}
	}
}

type toolSearch struct {
	h      *Host
	tools  []*mcpTool
	loaded *sync.Map
	desc   string
}

// newToolSearch builds the catalog once: it only changes with the servers,
// so it keeps the provider's prompt cache for the session.
func newToolSearch(h *Host, list []*mcpTool, loaded *sync.Map) *toolSearch {
	byServer := map[string][]string{}
	for _, m := range list {
		byServer[m.srv.name] = append(byServer[m.srv.name], m.shortName())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Load deferred MCP tools, callable from your next step on. Pass the names you need, space-separated, up to %d per call, or keywords matched against names, descriptions and parameters.\n\nDeferred tools by server, each named mcp_<server>_<tool>:", mcpSearchMax)
	for _, server := range slices.Sorted(maps.Keys(byServer)) {
		b.WriteString("\n- " + server + ": " + strings.Join(slices.Sorted(slices.Values(byServer[server])), ", "))
	}
	return &toolSearch{h: h, tools: list, loaded: loaded, desc: b.String()}
}

func (t *toolSearch) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        tools.ToolSearch,
		Description: t.desc,
		Parameters:  map[string]any{"query": map[string]any{"type": "string", "description": "tool names, or keywords"}},
		Required:    []string{"query"},
	}
}

func (t *toolSearch) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (t *toolSearch) SetProviderOptions(fantasy.ProviderOptions) {}

func (t *toolSearch) Run(ctx context.Context, call fantasy.ToolCall) (reply fantasy.ToolResponse, err error) {
	var in struct {
		Query string `json:"query"`
	}
	if err := decodeInput(t.Info(), call.Input, &in); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if resp, stop, err := t.h.preCheck(tools.ToolSearch, call.Input); stop {
		return resp, err
	}
	defer t.h.postNotify(tools.ToolSearch, call.Input, &reply)
	var fresh, had []string
	for _, n := range searchMCP(t.tools, in.Query, t.loaded) {
		if _, dup := t.loaded.LoadOrStore(n, true); dup {
			had = append(had, n)
		} else {
			fresh = append(fresh, n)
		}
	}
	var lines []string
	if len(fresh) > 0 {
		lines = append(lines, "Loaded, callable from your next step on: "+strings.Join(fresh, ", "))
	}
	if len(had) > 0 {
		lines = append(lines, "Already loaded: "+strings.Join(had, ", "))
	}
	if len(lines) == 0 {
		return fantasy.NewTextResponse("No deferred MCP tool that is not loaded yet matches; this tool's description lists them all."), nil
	}
	return fantasy.NewTextResponse(strings.Join(lines, "\n")), nil
}

// searchMCP ranks tools by how many keywords their name, description and
// parameter names contain, an exact name above any count, and returns at
// most mcpSearchMax names. A tool already loaded counts only by exact name,
// so repeating a search reaches the next matches.
func searchMCP(list []*mcpTool, query string, loaded *sync.Map) []string {
	words := strings.Fields(strings.ToLower(query))
	type hit struct {
		name  string
		score int
	}
	var hits []hit
	for _, m := range list {
		name := mcpToolName(m.srv.name, m.tool.Name)
		text := strings.ToLower(name + " " + m.tool.Description + " " + strings.Join(slices.Collect(maps.Keys(m.props)), " "))
		score, exact := 0, false
		for _, w := range words {
			switch {
			case w == strings.ToLower(name) || w == strings.ToLower(m.shortName()):
				score, exact = score+len(words)+1, true
			case strings.Contains(text, w):
				score++
			}
		}
		if _, ok := loaded.Load(name); ok && !exact {
			continue
		}
		if score > 0 {
			hits = append(hits, hit{name, score})
		}
	}
	slices.SortStableFunc(hits, func(a, b hit) int { return b.score - a.score })
	var out []string
	for _, hit := range hits[:min(len(hits), mcpSearchMax)] {
		out = append(out, hit.name)
	}
	return out
}

// shortName is the tool's name without its mcp_<server>_ prefix.
func (t *mcpTool) shortName() string {
	return strings.TrimPrefix(mcpToolName(t.srv.name, t.tool.Name), mcpToolPrefix+t.srv.name+"_")
}

// mcpMaxImage is the read tool's ceiling on an image read at all;
// tools.FitImage sizes what is sent. mcpMaxMedia caps an audio part, which
// nothing shrinks.
const (
	mcpMaxImage = 20 << 20
	mcpMaxMedia = 5 << 20
)

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

func (t *mcpTool) Run(ctx context.Context, call fantasy.ToolCall) (reply fantasy.ToolResponse, err error) {
	h, name := t.h, t.srv.name
	if resp, stop, err := h.preCheck(mcpToolName(name, t.tool.Name), call.Input); stop {
		return resp, err
	}
	defer h.postNotify(mcpToolName(name, t.tool.Name), call.Input, &reply)
	args, err := tools.ParseInput(t.Info(), call.Input, tools.SchemaOpen(t.tool.InputSchema))
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	req := tools.Request{Tool: mcpToolName(name, t.tool.Name), Subject: "mcp." + name, Arg: t.tool.Name, Preview: call.Input}
	if v := t.pipe.CheckDenied(req); v.Decision != perm.Allow {
		return fantasy.NewTextErrorResponse(tools.DeniedText(v, "mcp."+name+" "+t.tool.Name)), nil
	}
	// by name, not t.srv: a restart or reload mid-turn replaced the server
	s, _ := h.mcpServerNamed(name)
	var client *mcp.Client
	if s != nil && s.offered(t.tool.Name) {
		client = s.currentClient()
	}
	if client == nil {
		return fantasy.NewTextErrorResponse("MCP server " + name + " is not running or no longer offers " + t.tool.Name + " (/mcp shows why)"), nil
	}
	timeout := s.spec.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := client.CallTool(cctx, t.tool.Name, args)
	if errors.Is(err, mcp.ErrAuthRequired) {
		s.needsSignIn(h)
		return fantasy.NewTextErrorResponse("MCP server " + name + " needs the user to sign in again (/mcp)"), nil
	}
	if err != nil {
		return fantasy.NewTextErrorResponse("mcp " + name + ": " + err.Error()), nil
	}
	text := h.truncOutput("mcp", res.Text())
	if res.IsError {
		return fantasy.NewTextErrorResponse(text), nil
	}
	if strings.TrimSpace(text) == "" {
		text = "(no output)"
	}
	if data, mime, ok := res.Media(); ok {
		return mcpMedia(text, data, mime, t.pipe.Images), nil
	}
	return fantasy.NewTextResponse(text), nil
}

// mcpMedia sends a result's image or audio part with its text. An image
// goes through tools.FitImage like the read tool's: one a provider refuses
// would drop every image in the history, or all images for the process.
func mcpMedia(text string, data []byte, mime string, images func() bool) fantasy.ToolResponse {
	kind, _, _ := strings.Cut(mime, "/")
	limit := mcpMaxMedia
	if kind == "image" {
		limit = mcpMaxImage
	}
	var err error
	switch {
	case len(data) > limit:
		err = fmt.Errorf("a %d byte %s, over the %d byte limit", len(data), kind, limit)
	case kind != "image":
		out := fantasy.NewMediaResponse(data, mime)
		out.Content = text
		return out
	case images != nil && !images():
		err = errors.New("this model takes no images")
	default:
		if data, mime, err = tools.FitImage(data, mime); err == nil {
			out := fantasy.NewImageResponse(data, mime)
			out.Content = text
			return out
		}
	}
	return fantasy.NewTextResponse(text + "\n(the " + kind + " is not shown: " + err.Error() + ")")
}

// ---------------------------------------------------------------------------
// zemi.mcp and /mcp

func (h *Host) mcpNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	// add(name, { command, args, env, url, headers, client_id,
	// client_secret, callback_port, enabled_tools, disabled_tools, timeout }): user config
	// only, like providers.
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		opts := L.CheckTable(2)
		if h.RT.CurrentOrigin(L) != lua.OriginUser {
			L.RaiseError("zemi.mcp.add: only user config may add MCP servers")
		}
		spec := MCPSpec{
			Command: rawString(opts, "command"), Args: rawStrings(opts, "args"), Env: rawStringMap(opts, "env"),
			URL: rawString(opts, "url"), Headers: rawStringMap(opts, "headers"),
			ClientID: rawString(opts, "client_id"), ClientSecret: rawString(opts, "client_secret"),
			EnabledTools: rawStrings(opts, "enabled_tools"), DisabledTools: rawStrings(opts, "disabled_tools"),
			Source: "init.lua",
		}
		if n, ok := opts.RawGetString("callback_port").(glua.LNumber); ok {
			spec.CallbackPort = int(n)
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
			names := []string{}
			for _, t := range s.tools {
				if s.offered(t.Name) {
					names = append(names, t.Name)
				}
			}
			rows = append(rows, map[string]any{"name": s.name, "status": s.status, "error": s.err, "source": s.spec.Source, "tools": names})
			s.mu.Unlock()
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))
	return tb
}

// mcpCommands is /mcp.
func (h *Host) mcpCommands() []*Command {
	return []*Command{{
		Name: "mcp", NArgs: "*", Desc: "manage MCP servers",
		GoFn: func(h *Host, args string) error {
			sub, name, _ := strings.Cut(strings.TrimSpace(args), " ")
			switch sub {
			case "":
				return h.openMCP()
			case "restart":
				name = strings.TrimSpace(name)
				if err := h.restartMCP(name); err != nil {
					return err
				}
				h.Notify("info", "MCP", "restarting "+name)
				return nil
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
	}}
}

func (h *Host) restartMCP(name string) error {
	s, err := h.mcpServerNamed(name)
	if err != nil {
		return err
	}
	return h.addMCP(name, s.spec)
}

func (h *Host) openMCP() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	var items []any
	for _, s := range h.mcp.list() {
		s.mu.Lock()
		status, errText := s.status, s.err
		var tools []string
		for _, t := range s.tools {
			if s.offered(t.Name) {
				tools = append(tools, t.Name)
			}
		}
		all := len(s.tools)
		s.mu.Unlock()
		desc := status
		if status == "ready" {
			desc = fmt.Sprintf("ready · %d %s", len(tools), plural(len(tools), "tool", "tools"))
			if len(tools) < all {
				desc = fmt.Sprintf("ready · %d of %d tools", len(tools), all)
			}
		}
		detail := "command: " + strings.Join(append([]string{s.spec.Command}, s.spec.Args...), " ")
		if s.spec.URL != "" {
			detail = "url: " + s.spec.URL
		}
		detail += "\nstatus: " + status
		if errText != "" {
			detail += "\nerror: " + strings.TrimPrefix(errText, "mcp: ")
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
	return &mcp.Auth{Name: s.name, ServerURL: s.spec.URL, Path: filepath.Join(h.Dirs.DataHome, "mcp-auth", s.name+".json"),
		ClientID: s.spec.ClientID, ClientSecret: s.spec.ClientSecret, CallbackPort: s.spec.CallbackPort}
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
			h.Notify("error", "MCP", "signing in to "+name+" failed: "+err.Error())
			return
		}
		h.Notify("info", "MCP", "signed in to "+name)
		if err := h.restartMCP(name); err != nil {
			h.Notify("error", "MCP", err.Error())
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
	h.Notify("info", "MCP", "signed out of "+name)
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
