// mcp.go bridges MCP servers (DESIGN §11). User config adds them with
// zemi.mcp.add; each starts in the background, and every tool it lists
// becomes a model tool named mcp_<server>_<tool>, gated as subject
// mcp.<server> with the tool name as argument. Descriptions are labeled
// with their server: they are third-party text.

package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// mcpSpec is one server registration.
type mcpSpec struct {
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
}

type mcpServer struct {
	name string
	spec mcpSpec

	mu      sync.Mutex
	client  *mcp.Client
	tools   []mcp.Tool
	status  ServiceStatus
	err     string
	signIn  string // the sign-in URL while one waits in the browser
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
func (h *Host) addMCP(name string, spec mcpSpec) error {
	if !mcpServerName.MatchString(name) {
		return fmt.Errorf("server name %q must match [a-z0-9][a-z0-9-]{0,31}", name)
	}
	if (spec.Command == "") == (spec.URL == "") {
		return errors.New("give exactly one of command or url")
	}
	s := &mcpServer{name: name, spec: spec, status: StatusStarting}
	if h.Inert {
		s.status = StatusOff
	}
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
	if !h.Inert {
		h.GoSafe("mcp start "+name, func() { h.startMCP(s) })
	}
	return nil
}

// keeps reports whether s can serve spec as it is: a failed server restarts,
// one waiting for a sign-in keeps waiting, since a start would fail the same way.
func (s *mcpServer) keeps(spec mcpSpec) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return (s.status == StatusReady || s.status == StatusStarting || s.status == StatusSignIn) && reflect.DeepEqual(s.spec, spec)
}

func (h *Host) startMCP(s *mcpServer) {
	// a send before the server is ready runs without its tools
	defer h.Busy("Starting "+s.name, nil)()
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
			h.needsSignIn(s)
			return
		}
		if !h.setMCP(s, StatusFailed, err.Error()) {
			return
		}
		h.Log.Warn("server failed", "origin", "mcp", "server", s.name, "error", err)
		h.RecordError("mcp "+s.name, err.Error())
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
	s.client, s.tools = client, list // unused until the status is ready
	s.mu.Unlock()
	if !h.setMCP(s, StatusReady, "") {
		client.Close()
		return
	}
	h.Log.Info("server ready", "origin", "mcp", "server", s.name, "tools", len(list))
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
		h.Log.Warn("tool list refresh failed", "origin", "mcp", "server", s.name, "error", err)
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

// setMCP moves s to status and shows it, unless s was stopped (replaced or
// reloaded away, so its news is no one's) or is there already. It reports
// whether s moved.
func (h *Host) setMCP(s *mcpServer, status ServiceStatus, err string) bool {
	s.mu.Lock()
	moved := !s.stopped && (s.status != status || s.err != err)
	if moved {
		s.status, s.err = status, err
	}
	s.mu.Unlock()
	if moved {
		h.mcpChanged()
	}
	return moved
}

// mcpChanged redraws what shows the servers, an open /mcp included.
func (h *Host) mcpChanged() {
	h.refresh()
	if ui := h.hooks(); ui != nil {
		ui.OpenOverlay(h.mcpOverlay(true))
	}
}

func (h *Host) needsSignIn(s *mcpServer) {
	if h.setMCP(s, StatusSignIn, "") {
		h.Notify(slog.LevelWarn, "Sign-in needed", s.name+"; open /mcp")
	}
}

// offered reports whether enabled_tools and disabled_tools give the model
// the tool.
func (s *mcpServer) offered(name string) bool {
	return (s.spec.EnabledTools == nil || slices.Contains(s.spec.EnabledTools, name)) && !slices.Contains(s.spec.DisabledTools, name)
}

func (s *mcpServer) currentClient() *mcp.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != StatusReady {
		return nil
	}
	return s.client
}

func (s *mcpServer) stop() {
	s.mu.Lock()
	s.stopped, s.status = true, StatusStopped
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
	w.h.Log.Debug("stderr: "+truncRunes(strings.TrimSpace(string(p)), 500), "origin", w.source, "server", w.server)
	return len(p), nil
}

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

// mcpReadOnly reports a tool the server marked read-only and not
// destructive. It is the server's own word, which is the trust the user
// already extends by adding the server (zemi.mcp.add is user config only).
func (h *Host) mcpReadOnly(server, tool string) bool {
	s, _ := h.mcpServerNamed(server)
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.tools, func(t mcp.Tool) bool { return t.Name == tool })
	return i >= 0 && s.tools[i].ReadOnly()
}

// mcpFantasyTools exposes the ready servers' tools, checked through pipe.
func (h *Host) mcpFantasyTools(pipe *tools.Pipeline) []fantasy.AgentTool {
	var out []fantasy.AgentTool
	seen := map[string]bool{} // "a.b" and "a_b" map to one tool name
	for _, s := range h.mcp.list() {
		s.mu.Lock()
		list, ready := s.tools, s.status == StatusReady
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
				h.Log.Warn("tool name collides, skipped", "origin", "mcp", "server", s.name, "tool", t.Name)
				continue
			}
			seen[name] = true
			props, err := tools.ResolveSchemaRefs(t.InputSchema)
			if err != nil {
				h.Log.Warn("tool schema cannot be flattened, skipped", "origin", "mcp",
					"server", s.name, "tool", t.Name, "error", err,
				)
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
	tools.NoProviderOptions
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

func (t *toolSearch) Run(ctx context.Context, call fantasy.ToolCall) (reply fantasy.ToolResponse, err error) {
	var in struct {
		Query string `json:"query"`
	}
	if err := decodeInput(t.Info(), call.Input, &in); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if resp, stop := t.h.preCheck(tools.ToolSearch, call.Input); stop {
		return resp, nil
	}
	// every deferred tool shares the run's pipeline
	defer t.h.postNotify(ctx, t.tools[0].pipe.Pad, tools.ToolSearch, call.Input, &reply, time.Now())
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
	tools.NoProviderOptions
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

func (t *mcpTool) Run(ctx context.Context, call fantasy.ToolCall) (reply fantasy.ToolResponse, err error) {
	h, name, tool := t.h, t.srv.name, mcpToolName(t.srv.name, t.tool.Name)
	if resp, stop := h.preCheck(tool, call.Input); stop {
		return resp, nil
	}
	defer h.postNotify(ctx, t.pipe.Pad, tool, call.Input, &reply, time.Now())
	args, err := tools.ParseInput(t.Info(), call.Input, tools.SchemaOpen(t.tool.InputSchema))
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	req := tools.Request{Tool: tool, Subject: "mcp." + name, Arg: t.tool.Name, Preview: call.Input}
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
	cctx, cancel := context.WithTimeout(ctx, cmp.Or(s.spec.Timeout, 2*time.Minute))
	defer cancel()
	res, err := client.CallTool(cctx, t.tool.Name, args)
	if errors.Is(err, mcp.ErrAuthRequired) {
		h.needsSignIn(s)
		return fantasy.NewTextErrorResponse("MCP server " + name + " needs the user to sign in again (/mcp)"), nil
	}
	if err != nil {
		return fantasy.NewTextErrorResponse("mcp " + name + ": " + err.Error()), nil
	}
	text := h.truncOutput(t.pipe.Pad, "mcp", res.Text())
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
		spec := mcpSpec{
			Command: rawString(L, opts, "command"), Args: rawStrings(L, opts, "args"), Env: rawStringMap(L, opts, "env"),
			URL: rawString(L, opts, "url"), Headers: rawStringMap(L, opts, "headers"),
			ClientID: rawString(L, opts, "client_id"), ClientSecret: rawString(L, opts, "client_secret"),
			EnabledTools: rawStrings(L, opts, "enabled_tools"), DisabledTools: rawStrings(L, opts, "disabled_tools"),
		}
		if n, ok := opt[glua.LNumber](L, opts, "callback_port"); ok {
			spec.CallbackPort = int(n)
		}
		if n, ok := opt[glua.LNumber](L, opts, "timeout"); ok && n > 0 {
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
			rows = append(rows, map[string]any{"name": s.name, "status": string(s.status), "error": s.err, "tools": names})
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
		Name: "mcp", NArgs: NArgsAny, Desc: "manage MCP servers",
		GoFn: func(h *Host, args string) error {
			sub, name, _ := strings.Cut(strings.TrimSpace(args), " ")
			switch sub {
			case "":
				return h.openMCP("")
			case "restart":
				name = strings.TrimSpace(name)
				if err := h.restartMCP(name); err != nil {
					return err
				}
				h.Notify(slog.LevelInfo, "Server restarting", name)
				return nil
			case "sign-in":
				return h.signInMCP(strings.TrimSpace(name))
			case "sign-out":
				name, yes := strings.CutSuffix(strings.TrimSpace(name), " --yes")
				return h.signOutMCP(name, yes)
			}
			return Usagef("unknown /mcp subcommand %q", sub)
		},
		GoComplete: SubcommandCompleter([]string{"restart", "sign-in", "sign-out"}, func(h *Host, _ string) []string {
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

// openMCP is /mcp; confirm arms the action that runs it (Overlay.Confirm).
func (h *Host) openMCP(confirm string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	o := h.mcpOverlay(false)
	o.Confirm = confirm
	ui.OpenOverlay(o)
	return nil
}

// mcpOverlay is /mcp; with refresh it only replaces an open one.
func (h *Host) mcpOverlay(refresh bool) Overlay {
	return Overlay{
		ID: "mcp", Title: "MCP servers", Origin: lua.OriginBuiltin, Refresh: refresh, Items: h.mcpItems(),
		Reload: func() ([]any, [][]Segment) { return h.mcpItems(), nil },
		Empty:  `No MCP servers yet. Add one in init.lua: zemi.mcp.add("name", { command = "..." }).`,
	}
}

func (h *Host) mcpItems() []any {
	var items []any
	for _, s := range h.mcp.list() {
		s.mu.Lock()
		status, errText, signIn := s.status, s.err, s.signIn
		var tools []string
		for _, t := range s.tools {
			if s.offered(t.Name) {
				tools = append(tools, t.Name)
			}
		}
		all := len(s.tools)
		s.mu.Unlock()
		desc := string(status)
		if status == StatusReady {
			desc = fmt.Sprintf("ready · %d %s", len(tools), Plural(len(tools), "tool", "tools"))
			if len(tools) < all {
				desc = fmt.Sprintf("ready · %d of %d tools", len(tools), all)
			}
		}
		where := []string{"command", strings.Join(append([]string{s.spec.Command}, s.spec.Args...), " ")}
		if s.spec.URL != "" {
			where = []string{"url", s.spec.URL}
		}
		if len(tools) > 20 {
			tools = append(tools[:20], "…")
		}
		fields := fieldList(where[0], where[1], "status", string(status), "sign in at", signIn,
			"tools", strings.Join(tools, ", "), "error", strings.TrimPrefix(errText, "mcp: "))
		actions := []any{map[string]any{"text": "Restart", "hotkey": "r", "run": "mcp restart " + s.name}}
		if status == StatusSignIn {
			actions = append(actions, map[string]any{"text": "Sign in", "hotkey": "s", "run": "mcp sign-in " + s.name})
		}
		if _, err := os.Stat(h.mcpAuth(s).Path); s.spec.URL != "" && err == nil {
			actions = append(actions, map[string]any{"text": "Sign out", "hotkey": "x", "danger": true, "run": "mcp sign-out " + s.name + " --yes"})
		}
		items = append(items, map[string]any{"text": s.name, "desc": desc, "fields": fields, "actions": actions})
	}
	return items
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
	return &mcp.Auth{
		Name: s.name, ServerURL: s.spec.URL, Path: filepath.Join(h.Dirs.DataHome, "mcp-auth", s.name+".json"),
		ClientID: s.spec.ClientID, ClientSecret: s.spec.ClientSecret, CallbackPort: s.spec.CallbackPort,
	}
}

func (h *Host) mcpServerNamed(name string) (*mcpServer, error) {
	h.mcp.mu.Lock()
	s := h.mcp.servers[name]
	h.mcp.mu.Unlock()
	if s == nil {
		return nil, Usagef("no MCP server %q", name)
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
		return Usagef("%s runs a local command; only HTTP servers sign in", name)
	}
	auth := h.mcpAuth(s)
	h.GoSafe("mcp sign-in "+name, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err := auth.Authorize(ctx, func(u string) {
			// too long for a toast: /mcp shows it whole, to copy
			s.mu.Lock()
			s.signIn = u
			s.mu.Unlock()
			h.mcpChanged()
			h.Notify(slog.LevelInfo, "Browser opened", "sign in to "+name+" there, or open /mcp for the link")
			h.openBrowser(u)
		})
		s.mu.Lock()
		s.signIn = ""
		s.mu.Unlock()
		h.mcpChanged()
		if err != nil {
			h.RecordError("mcp "+name, err.Error())
			h.Notify(slog.LevelError, "Could not sign in to "+name, err.Error())
			return
		}
		h.Notify(slog.LevelInfo, "Signed in", name)
		if err := h.restartMCP(name); err != nil {
			h.Notify(slog.LevelError, "Could not restart "+name, err.Error())
		}
	})
	return nil
}

// signOutMCP deletes a server's saved token once yes confirms it (AskFirst).
func (h *Host) signOutMCP(name string, yes bool) error {
	s, err := h.mcpServerNamed(name)
	if err != nil {
		return err
	}
	auth := h.mcpAuth(s)
	if _, err := os.Stat(auth.Path); s.spec.URL == "" || err != nil {
		return Usagef("%s is not signed in", name)
	}
	if !yes {
		return AskFirst("mcp sign-out "+name+" --yes", "", h.openMCP)
	}
	if err := auth.Logout(); err != nil {
		return err
	}
	// NOTE: a start is assumed to fail the same way, so none runs; a server
	// whose auth is optional waits for a sign-in until Restart.
	out := &mcpServer{name: name, spec: s.spec, status: StatusSignIn}
	h.mcp.mu.Lock()
	// a reload may have replaced or dropped s since; that server stays
	swapped := h.mcp.servers[name] == s
	if swapped {
		h.mcp.servers[name] = out
	}
	h.mcp.mu.Unlock()
	if swapped {
		s.stop()
	}
	h.Notify(slog.LevelInfo, "Signed out", name)
	h.mcpChanged()
	return nil
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
