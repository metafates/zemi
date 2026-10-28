// mcp.go bridges MCP servers (DESIGN §11). User config adds them with
// zemi.mcp.add; each starts in the background, and every tool it lists
// becomes a model tool named mcp_<server>_<tool>, gated as subject
// mcp.<server> with the tool name as argument. Descriptions are labeled
// with their server: they are third-party text.

package hostapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/mcp"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/set"
	"github.com/metafates/zemi/internal/tools"
)

// mcpSpec is one server registration.
type mcpSpec struct {
	Group         string
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

	mu          sync.Mutex
	client      *mcp.Client
	tools       []mcp.Tool
	status      ServiceStatus
	enabled     bool // the user's persisted switch, off never starts
	err         string
	signIn      string // the sign-in URL while one waits in the browser
	authorizing bool   // a flow is between its start and its park, so no second one starts
	signedIn    bool   // a stored grant existed the last time the keychain was asked
	stopped     bool
	refreshing  bool // a tools/list refresh is running
	queued      bool // a list_changed landed during it: one more run after
}

type mcpReg struct {
	mu      sync.Mutex
	servers map[string]*mcpServer
	parked  map[string]*mcpServer // the replaced generation's, until its replay is done
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
	if err := h.checkMCPGroupName(name, spec.Group); err != nil {
		return err
	}
	enabled := h.mcpEnabled(name)
	s := &mcpServer{name: name, spec: spec, status: StatusStarting, enabled: enabled}
	if h.Inert || !enabled {
		s.status = StatusOff
	}
	h.mcp.mu.Lock()
	if h.mcp.servers == nil {
		h.mcp.servers = map[string]*mcpServer{}
	}
	old, parked := h.mcp.servers[name], h.mcp.parked[name]
	delete(h.mcp.parked, name)
	// a reload that adds a live server again keeps it: a restart would lose
	// its state (a browser, a connection) and move the tool list. A disabled
	// switch never adopts one.
	if old == nil && parked != nil && enabled && parked.keeps(spec) {
		h.mcp.servers[name] = parked
		h.mcp.mu.Unlock()
		if parked.group() != spec.Group {
			parked.adoptGroup(spec.Group)
			h.mcpChanged()
		}
		return nil
	}
	h.mcp.servers[name] = s
	// the replaced servers stop inside the section: two racing adds each read
	// the other's insert as old, so the loser's child never runs on
	for _, o := range []*mcpServer{old, parked} {
		if o != nil {
			o.stop()
		}
	}
	h.mcp.mu.Unlock()
	if spec.URL != "" {
		// the row's sign-out key rests on this one fact, and the keychain
		// subprocess is far too slow to ask on every /mcp build
		h.GoSafe("mcp grant "+name, func() { h.refreshMCPSignIn(s) })
	}
	// a racing add for the same name may have stopped s between the sections
	s.mu.Lock()
	starting := s.status == StatusStarting
	s.mu.Unlock()
	if starting {
		h.GoSafe("mcp start "+name, func() { h.startMCP(s) })
	}
	return nil
}

// checkMCPGroupName refuses a group that would make one name both a server
// and a group: toggle resolves a server first, so a colliding group would be
// unreachable. Parked servers stay out: the config's final state decides.
func (h *Host) checkMCPGroupName(name, group string) error {
	if group != "" {
		if !mcpServerName.MatchString(group) {
			return fmt.Errorf("group name %q must match [a-z0-9][a-z0-9-]{0,31}", group)
		}
		if group == name {
			return fmt.Errorf("group %q cannot be the server's own name", group)
		}
	}
	h.mcp.mu.Lock()
	defer h.mcp.mu.Unlock()
	for _, s := range h.mcp.servers {
		if group != "" && s.name == group {
			return fmt.Errorf("group %q is already a server's name", group)
		}
		if s.group() == name {
			return fmt.Errorf("server name %q is already a group's name", name)
		}
	}
	return nil
}

// kvMCPPrefix is the kv namespace an MCP server's persisted switch lives
// under.
const kvMCPPrefix = "mcp:"

// mcpEnabled reads a server's persisted switch; no row, or no store, is on.
func (h *Host) mcpEnabled(name string) bool {
	if h.GlobalDB == nil {
		return true
	}
	v, err := h.GlobalDB.GetKV(kvMCPPrefix + name)
	if err != nil {
		return true
	}
	return !kvFalse(v)
}

// setMCPEnabled writes a server's persisted switch.
func (h *Host) setMCPEnabled(name string, on bool) error {
	if h.GlobalDB == nil {
		return errors.New("MCP state store unavailable")
	}
	if on {
		_, err := h.GlobalDB.DelKV(kvMCPPrefix + name)
		return err
	}
	return h.GlobalDB.SetKV(kvMCPPrefix+name, "false")
}

// kvFalse reports a kv value that is the JSON false, never a string
// comparison: user Lua shares the kv table and stores "false" quoted, and
// null is not off.
func kvFalse(v string) bool {
	var anyv any
	return json.Unmarshal([]byte(v), &anyv) == nil && anyv == false
}

// setMCPOn flips one server's persisted switch and starts or stops it to
// match. The persisted side moves first, so a crash between the two lands on
// it; a server already in the wanted state only picks up the switch.
func (h *Host) setMCPOn(name string, on bool) error {
	s, err := h.mcpServerNamed(name)
	if err != nil {
		return err
	}
	if err := h.setMCPEnabled(name, on); err != nil {
		return err
	}
	s.mu.Lock()
	running := s.status != StatusOff
	s.mu.Unlock()
	if on == running {
		return nil
	}
	return h.addMCP(name, s.specSnapshot())
}

// mcpTarget resolves a toggle name to its server, else to the servers of the
// group by that name, in name order.
func (h *Host) mcpTarget(name string) (*mcpServer, []*mcpServer) {
	h.mcp.mu.Lock()
	defer h.mcp.mu.Unlock()
	if s := h.mcp.servers[name]; s != nil {
		return s, nil
	}
	var group []*mcpServer
	for _, s := range h.mcp.servers {
		if s.group() == name {
			group = append(group, s)
		}
	}
	slices.SortFunc(group, func(a, b *mcpServer) int { return cmp.Compare(a.name, b.name) })
	return nil, group
}

// setMCPName sets one server, or a whole group, to on or off.
func (h *Host) setMCPName(name string, on bool) error {
	s, group := h.mcpTarget(name)
	if s != nil {
		return h.setMCPOn(name, on)
	}
	if len(group) == 0 {
		return Usagef("no MCP server or group %q", name)
	}
	for _, m := range group {
		// by name each step: a member a reload dropped mid-toggle is skipped
		if _, err := h.mcpServerNamed(m.name); err != nil {
			continue
		}
		if err := h.setMCPOn(m.name, on); err != nil {
			return err
		}
	}
	return nil
}

// toggleMCPName flips one server, or a whole group at once: any member off
// and the group comes on together, all on and it goes off together.
func (h *Host) toggleMCPName(name string) error {
	s, group := h.mcpTarget(name)
	switch {
	case s != nil:
		return h.setMCPOn(name, !s.enabledFlag())
	case len(group) > 0:
		return h.setMCPName(name, slices.ContainsFunc(group, func(m *mcpServer) bool { return !m.enabledFlag() }))
	default:
		return Usagef("no MCP server or group %q", name)
	}
}

// signInTimeout is how long one sign-in flow may take.
const signInTimeout = 5 * time.Minute

// keeps reports whether s can serve spec as it is: a failed server restarts,
// one waiting for a sign-in keeps waiting, since a start would fail the same
// way. The group is a label, not connection state, so a moved group adopts
// instead of restarting.
func (s *mcpServer) keeps(spec mcpSpec) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	mine, theirs := s.spec, spec
	mine.Group, theirs.Group = "", ""
	return (s.status == StatusReady || s.status == StatusStarting || s.status == StatusSignIn) && reflect.DeepEqual(mine, theirs)
}

// enabledFlag is the server's persisted switch.
func (s *mcpServer) enabledFlag() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

// group is the server's group name; the group moves at runtime, so the spec
// is read only under the lock.
func (s *mcpServer) group() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spec.Group
}

// specSnapshot copies the spec under the lock.
func (s *mcpServer) specSnapshot() mcpSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spec
}

// adoptGroup takes a spec's new group without a restart.
func (s *mcpServer) adoptGroup(group string) {
	s.mu.Lock()
	s.spec.Group = group
	s.mu.Unlock()
}

func (h *Host) startMCP(s *mcpServer) {
	// a send before the server is ready runs without its tools
	defer h.Busy("Starting "+s.name, nil)()
	env := append([]string{}, os.Environ()...)
	env = append(env, "ZEMI_PROJECT_DIR="+h.Dirs.ProjectDir)
	for k, v := range s.spec.Env {
		env = append(env, k+"="+v)
	}
	// Outside the project even when it is trusted: npx and uv read .npmrc,
	// node_modules/.bin and uv config from the working directory, which the
	// model's writes may change. ZEMI_PROJECT_DIR says where the project is.
	headers := map[string]string{}
	maps.Copy(headers, s.spec.Headers)
	cfg := mcp.Config{
		Command: s.spec.Command, Args: s.spec.Args, Env: env, Dir: h.Dirs.DataHome,
		Stderr: serverLogWriter{h: h, source: "mcp", server: s.name},
		URL:    s.spec.URL, Headers: headers,
		OnToolsChanged: func() { h.queueMCPRefresh(s) },
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
		h.RecordError(errMCP, s.name, err.Error())
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
	h.mcpChanged()
}

// queueMCPRefresh coalesces notifications/tools/list_changed: one refresh
// runs at a time, and a notification landing during it buys one more run
// after, so a spamming server cannot fan out goroutines and tools/list calls.
func (h *Host) queueMCPRefresh(s *mcpServer) {
	s.mu.Lock()
	if s.refreshing {
		s.queued = true
		s.mu.Unlock()
		return
	}
	s.refreshing = true
	s.mu.Unlock()
	h.GoSafe("mcp tools "+s.name, func() {
		for {
			h.refreshMCPTools(s)
			s.mu.Lock()
			again := s.queued
			s.queued = false
			if !again {
				s.refreshing = false
			}
			s.mu.Unlock()
			if !again {
				return
			}
		}
	})
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
		h.RecordErrorOnce(errMCP, s.name, "enabled_tools or disabled_tools names no tool the server lists: "+strings.Join(unknown, ", "))
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

// mcpChanged redraws what shows the servers, an open /mcp and its open group
// level included. The refresh builds nothing for a closed one.
func (h *Host) mcpChanged() {
	h.refresh()
	if ui := h.hooks(); ui != nil {
		ui.RefreshOverlay("mcp", h.mcpOverlay)
		if group := h.mcpOpenGroup(); group != "" {
			ui.RefreshOverlay("mcp group "+group, func() Overlay { return h.mcpGroupOverlay(group) })
		}
	}
}

// mcpOpenGroup is the group whose level /mcp opened last, "" for none: a
// closed one builds nothing (RefreshOverlay draws only open overlays).
func (h *Host) mcpOpenGroup() string {
	h.msgMu.Lock()
	defer h.msgMu.Unlock()
	return h.mcpLevel
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
	if r.parked == nil {
		r.parked = map[string]*mcpServer{}
	}
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
// already extends by adding the server.
// mcpOfferedTool is one MCP tool a run gets, under the name a call uses.
type mcpOfferedTool struct {
	srv   *mcpServer
	tool  mcp.Tool
	name  string
	props map[string]any // input schema properties, local $refs resolved
}

// mcpOffered is what the ready servers offer a run, in one order: two tools
// mapping to one name leave the first, and a schema that cannot be flattened
// drops its tool, so every name here is one a call reaches.
func (h *Host) mcpOffered() []mcpOfferedTool {
	var out []mcpOfferedTool
	seen := set.Set[string]{} // "a.b" and "a_b" map to one tool name
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
			if seen.Has(name) {
				h.Log.Warn("tool name collides, skipped", "origin", "mcp", "server", s.name, "tool", t.Name)
				continue
			}
			seen.Add(name)
			props, err := tools.ResolveSchemaRefs(t.InputSchema)
			if err != nil {
				h.Log.Warn("tool schema cannot be flattened, skipped", "origin", "mcp",
					"server", s.name, "tool", t.Name, "error", err,
				)
				continue
			}
			out = append(out, mcpOfferedTool{srv: s, tool: t, name: name, props: props})
		}
	}
	return out
}

// mcpFantasyTools exposes the ready servers' tools, checked through pipe.
func (h *Host) mcpFantasyTools(pipe *tools.Pipeline) []fantasy.AgentTool {
	offered := h.mcpOffered()
	out := make([]fantasy.AgentTool, 0, len(offered))
	for _, o := range offered {
		out = append(out, &mcpTool{h: h, srv: o.srv, tool: o.tool, pipe: pipe, props: o.props})
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
	always = append(always, string(tools.ToolSearch))
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

// toolSearchArgs is the tool_search tool's input and its schema source.
type toolSearchArgs struct {
	Query string `json:"query" desc:"tool names, or keywords"`
}

func (t *toolSearch) Info() fantasy.ToolInfo {
	props, required := tools.ArgsOf(toolSearchArgs{})
	return fantasy.ToolInfo{
		Name:        string(tools.ToolSearch),
		Description: t.desc,
		Parameters:  props,
		Required:    required,
	}
}

func (t *toolSearch) Run(ctx context.Context, call fantasy.ToolCall) (reply fantasy.ToolResponse, err error) {
	var in toolSearchArgs
	if err := decodeInput(t.Info(), call.Input, &in); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if resp, stop := t.h.preCheck(string(tools.ToolSearch), call.Input); stop {
		return resp, nil
	}
	// every deferred tool shares the run's pipeline
	defer t.h.postNotify(ctx, t.tools[0].pipe.Pad, string(tools.ToolSearch), call.Input, &reply, time.Now())
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
	req := tools.Request{Tool: tool, Subject: "mcp." + name, Arg: t.tool.Name, Preview: call.Input, Read: t.tool.ReadOnly()}
	if v := t.pipe.CheckDenied(req); !v.Allowed {
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
	text := h.truncScriptOutput(t.pipe, "mcp", res.Text())
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
	// client_secret, callback_port, enabled_tools, disabled_tools, timeout,
	// group }).
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		opts := L.CheckTable(2)
		spec := mcpSpec{
			Command: rawString(L, opts, "command"), Args: rawStrings(L, opts, "args"), Env: rawStringMap(L, opts, "env"),
			URL: rawString(L, opts, "url"), Headers: rawStringMap(L, opts, "headers"),
			ClientID: rawString(L, opts, "client_id"), ClientSecret: rawString(L, opts, "client_secret"),
			EnabledTools: rawStrings(L, opts, "enabled_tools"), DisabledTools: rawStrings(L, opts, "disabled_tools"),
			Group: rawString(L, opts, "group"),
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
			var names []string
			for _, t := range s.tools {
				if s.offered(t.Name) {
					names = append(names, t.Name)
				}
			}
			rows = append(rows, map[string]any{"name": s.name, "status": string(s.status), "error": s.err, "tools": names, "group": s.spec.Group, "enabled": s.enabled})
			s.mu.Unlock()
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))
	// del(name): remove and stop a server zemi.mcp.add registered; false when
	// none has that name.
	tb.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		h.mcp.mu.Lock()
		s, ok := h.mcp.servers[name]
		if ok {
			delete(h.mcp.servers, name)
			delete(h.mcp.parked, name)
		}
		h.mcp.mu.Unlock()
		if ok {
			s.stop()
			// the switch goes with the server, so a re-add starts fresh; a
			// del works where no store exists
			_ = h.setMCPEnabled(name, true)
		}
		L.Push(glua.LBool(ok))
		return 1
	}))
	// enable(name, on): flip a server's persisted switch, or a whole group's;
	// a name that is neither gives nil and the error, like zemi.skills.enable.
	// A call from init.lua pins the switch against every reload, like any
	// config write.
	tb.RawSetString("enable", L.NewFunction(func(L *glua.LState) int {
		return luaResult(L, h.setMCPName(L.CheckString(1), L.CheckBool(2)))
	}))
	return tb
}

// mcpCommands is /mcp.
func (h *Host) mcpCommands() []*Command {
	return []*Command{{
		Name: "mcp", NArgs: NArgsAny, Desc: "manage MCP servers",
		GoFn: func(h *Host, args string, confirm bool) error {
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
			case "enable", "disable", "toggle":
				name = strings.TrimSpace(name)
				if name == "" {
					return Usagef("usage: /mcp %s <server or group>", sub)
				}
				var err error
				if sub == "toggle" {
					err = h.toggleMCPName(name)
				} else {
					err = h.setMCPName(name, sub == "enable")
				}
				if err != nil {
					return err
				}
				h.Notify(slog.LevelInfo, "MCP "+sub+"d", name)
				return nil
			case "sign-in":
				return h.signInMCP(strings.TrimSpace(name))
			case "sign-in-all":
				return h.signInAllMCP()
			case "group":
				return h.openMCPGroup(strings.TrimSpace(name))
			case "sign-out":
				name, confirm = ConfirmArgs(name, confirm)
				return h.signOutMCP(name, confirm)
			}
			return Usagef("unknown /mcp subcommand %q", sub)
		},
		GoComplete: SubcommandCompleter([]string{"restart", "sign-in", "sign-in-all", "sign-out", "enable", "disable", "toggle", "group"}, func(h *Host, sub string) []string {
			var names []string
			for _, s := range h.mcp.list() {
				names = append(names, s.name)
				// the toggle verbs and group act on groups; restart and sign-in do not
				switch sub {
				case "enable", "disable", "toggle", "group":
					if g := s.group(); g != "" && !slices.Contains(names, g) {
						names = append(names, g)
					}
				}
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
	if !s.enabledFlag() {
		return Usagef("%s is off; turn it on in /mcp first", name)
	}
	return h.addMCP(name, s.specSnapshot())
}

// openMCP is /mcp; confirm arms the action that runs it (Overlay.Confirm).
func (h *Host) openMCP(confirm string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	o := h.mcpOverlay()
	o.Confirm = confirm
	ui.OpenOverlay(o)
	return nil
}

// mcpOverlay is /mcp. The batch key is there once two or more HTTP servers
// ask for a sign-in: one is the row's own Sign in.
func (h *Host) mcpOverlay() Overlay {
	o := Overlay{
		ID: "mcp", Title: "MCP servers", Origin: lua.OriginBuiltin, Items: h.mcpItems(),
		Reload: h.mcpItems,
		Empty:  `No MCP servers yet. Add one in init.lua: zemi.mcp.add("name", { command = "..." }).`,
	}
	if len(h.mcpWaitingNames()) >= 2 {
		o.Keys = []OverlayKey{{Key: "a", Desc: "sign in to all", Run: "mcp sign-in-all"}}
	}
	return o
}

// mcpWaitingNames names the HTTP servers whose sign-in no flow has started
// yet: the batch key offers once two of them wait, and the batch queue takes
// the same list, so the footer never offers a key the queue would refuse.
func (h *Host) mcpWaitingNames() []string {
	var names []string
	for _, s := range h.mcp.list() {
		s.mu.Lock()
		waiting := s.status == StatusSignIn && s.spec.URL != "" && s.signIn == "" && !s.authorizing
		s.mu.Unlock()
		if waiting {
			names = append(names, s.name)
		}
	}
	return names
}

// mcpMarks are the list rows' status marks, the chat's: starting spins and
// a server waiting for a sign-in waits.
var mcpMarks = map[ServiceStatus]MarkKind{
	StatusStarting: tools.MarkSpin, StatusReady: tools.MarkOK, StatusFailed: tools.MarkErr,
	StatusSignIn: tools.MarkWait, StatusStopped: tools.MarkStop, StatusOff: tools.MarkStop,
}

// mcpItems lists the servers: ungrouped ones flat, each group one row whose
// switch reads on only when every member is on, so space turns a mixed or
// off group on and an all-on group off; l opens the group's level, its
// servers listed there.
func (h *Host) mcpItems() []OverlayItem {
	servers := h.mcp.list()
	groups := map[string][]*mcpServer{}
	var solo []*mcpServer
	for _, s := range servers {
		if g := s.group(); g == "" {
			solo = append(solo, s)
		} else {
			groups[g] = append(groups[g], s)
		}
	}
	var items []OverlayItem
	for _, g := range slices.Sorted(maps.Keys(groups)) {
		members := groups[g]
		on := 0
		for _, m := range members {
			if m.enabledFlag() {
				on++
			}
		}
		items = append(items, OverlayItem{
			ID:   "mcp group " + g,
			Text: g,
			Row:  []Segment{{Text: g, HL: GroupTitle}},
			Desc: fmt.Sprintf("%d of %d on", on, len(members)),
			On:   new(on == len(members)),
			Run:  "mcp toggle " + g,
			Keys: []ItemKey{
				{Key: "l", Desc: "open", Run: "mcp group " + g, Verb: VerbOpen},
				{Key: "e", Desc: "enable all", Run: "mcp enable " + g},
				{Key: "d", Desc: "disable all", Run: "mcp disable " + g},
			},
		})
	}
	for _, s := range solo {
		items = append(items, h.mcpItem(s))
	}
	return items
}

// mcpGroupItems lists one group's servers as the rows its level toggles.
func (h *Host) mcpGroupItems(group string) []OverlayItem {
	var items []OverlayItem
	for _, s := range h.mcp.list() {
		if s.group() == group {
			items = append(items, h.mcpItem(s))
		}
	}
	return items
}

// mcpGroupOverlay is one group's level: its servers, toggled in place. The
// id extends the list's own, so the menu closes whole from any of its levels
// and esc lands back on the list.
func (h *Host) mcpGroupOverlay(group string) Overlay {
	return Overlay{
		ID:     "mcp group " + group,
		Title:  "MCP servers / " + group,
		Origin: lua.OriginBuiltin,
		Items:  h.mcpGroupItems(group),
		Reload: func() []OverlayItem { return h.mcpGroupItems(group) },
		Empty:  "No servers in this group.",
	}
}

// openMCPGroup opens one group's level; /mcp group <name>.
func (h *Host) openMCPGroup(group string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	if group == "" {
		return Usagef("usage: /mcp group <group>")
	}
	if len(h.mcpGroupItems(group)) == 0 {
		return Usagef("no MCP group %q", group)
	}
	ui.OpenOverlay(h.mcpGroupOverlay(group))
	h.msgMu.Lock()
	h.mcpLevel = group
	h.msgMu.Unlock()
	return nil
}

// mcpItem is one server row: the switch is the persisted switch, the mark and
// desc the runtime state, and enter still opens the detail.
func (h *Host) mcpItem(s *mcpServer) OverlayItem {
	s.mu.Lock()
	status, errText, signIn, signedIn := s.status, s.err, s.signIn, s.signedIn
	spec := s.spec
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
	} else if signIn != "" {
		desc = "waiting in the browser"
	}
	where := []string{"command", strings.Join(append([]string{spec.Command}, spec.Args...), " ")}
	if spec.URL != "" {
		where = []string{"url", spec.URL}
	}
	if len(tools) > 20 {
		tools = append(tools[:20], "…")
	}
	fields := fieldList(where[0], where[1], "status", string(status), "sign in at", signIn,
		"tools", strings.Join(tools, ", "), "error", strings.TrimPrefix(errText, "mcp: "))
	var keys []ItemKey
	if status != StatusOff { // a disabled server has nothing to restart
		keys = append(keys, ItemKey{Key: "r", Desc: "restart", Run: "mcp restart " + s.name})
	}
	if status == StatusSignIn {
		keys = append(keys, ItemKey{Key: "s", Desc: "sign in", Run: "mcp sign-in " + s.name})
	}
	if spec.URL != "" && signedIn {
		keys = append(keys, ItemKey{Key: "x", Desc: "sign out", Danger: true, Run: "mcp sign-out " + s.name})
	}
	item := OverlayItem{
		ID: "mcp " + s.name, Text: s.name, Desc: desc,
		Mark: mcpMarks[status], On: new(s.enabledFlag()), Run: "mcp toggle " + s.name,
		Fields: fields, Keys: keys,
	}
	if signIn != "" {
		// the URL is too long for a toast; y copies it when the browser
		// was blocked
		item.Copy = signIn
	}
	return item
}

func hasHeader(headers map[string]string, name string) bool {
	for k := range headers {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// mcpAuth is an HTTP server's stored sign-in; the grant lives in the OS
// keychain, and mcp-auth/ holds the fallback file.
func (h *Host) mcpAuth(s *mcpServer) *mcp.Auth {
	return &mcp.Auth{
		Name: s.name, ServerURL: s.spec.URL, Path: filepath.Join(h.Dirs.DataHome, "mcp-auth", s.name+".json"), DataHome: h.Dirs.DataHome,
		ClientID: s.spec.ClientID, ClientSecret: s.spec.ClientSecret, CallbackPort: s.spec.CallbackPort,
	}
}

// refreshMCPSignIn asks the keychain once whether a stored grant exists. It
// runs off the UI path at registration: /mcp builds a row per server, and
// the keychain subprocess would open it late. A grant saved in another
// window shows here only after the next add.
func (h *Host) refreshMCPSignIn(s *mcpServer) {
	_, _, ok := h.mcpAuth(s).Ref().Get()
	s.mu.Lock()
	moved := ok != s.signedIn
	s.signedIn = ok
	s.mu.Unlock()
	if moved {
		h.mcpChanged()
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

// errAuthorizing marks a server whose sign-in another flow already holds.
var errAuthorizing = errors.New("a sign-in flow is already running for this server")

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
	h.GoSafe("mcp sign-in "+name, func() {
		ctx, cancel := context.WithTimeout(context.Background(), signInTimeout)
		defer cancel()
		switch err := h.authorizeMCP(ctx, s); {
		case err == nil:
			h.signedInMCP(name)
		case errors.Is(err, errAuthorizing):
			h.Notify(slog.LevelInfo, "Already signing in to "+name, "one flow is waiting in the browser")
		}
	})
	return nil
}

// authorizeMCP runs one server's browser sign-in to completion: it parks the
// URL on the server for /mcp to show, opens the browser, and clears the URL
// when the flow ends either way. The context bounds the wait for the user.
// The claim of authorizing is the same lock section that checks it, so two
// presses of the batch key (or a row press beside the queue) cannot start a
// second flow for one server; the loser gets errAuthorizing. A failure the
// user did not cancel reports here, both callers sharing one toast.
func (h *Host) authorizeMCP(ctx context.Context, s *mcpServer) error {
	s.mu.Lock()
	if s.authorizing {
		s.mu.Unlock()
		return errAuthorizing
	}
	s.authorizing = true
	s.mu.Unlock()
	authorize := h.mcpAuth(s).Authorize
	if h.authFn != nil {
		authorize = h.authFn
	}
	err := authorize(ctx, func(u string) {
		// too long for a toast: /mcp shows it whole, to copy
		s.mu.Lock()
		s.signIn = u
		s.mu.Unlock()
		h.mcpChanged()
		h.Notify(slog.LevelInfo, "Browser opened", "sign in to "+s.name+" there, or open /mcp for the link")
		h.openBrowser(u)
	})
	s.mu.Lock()
	s.signIn, s.authorizing = "", false
	s.mu.Unlock()
	h.mcpChanged()
	if err != nil && !errors.Is(err, context.Canceled) {
		h.RecordError(errMCP, s.name, err.Error())
		h.Notify(slog.LevelError, "Could not sign in to "+s.name, err.Error())
	}
	return err
}

// signedInMCP restarts a server whose sign-in just succeeded, unless the user
// turned it off while the browser was open: the grant is saved either way.
func (h *Host) signedInMCP(name string) {
	if s, err := h.mcpServerNamed(name); err == nil && !s.enabledFlag() {
		return
	}
	h.Notify(slog.LevelInfo, "Signed in", name)
	if err := h.restartMCP(name); err != nil {
		h.Notify(slog.LevelError, "Could not restart "+name, err.Error())
	}
}

// signInAllMCP signs every HTTP server asking for a sign-in in, one at a
// time: the servers share zemi's callback ports, and two browsers at once
// lose to popup blockers. A server the user is already signing in, one that
// moved, or one a reload dropped is skipped; the queue never starts a server
// to force its 401.
func (h *Host) signInAllMCP() error {
	names := h.mcpWaitingNames()
	if len(names) == 0 {
		return Usagef("no MCP server is waiting for a sign-in")
	}
	h.GoSafe("mcp sign-in all", func() {
		signed, failed := 0, []string(nil)
		for _, name := range names {
			// by name each step: a reload replaces the servers mid-queue
			s, err := h.mcpServerNamed(name)
			if err != nil {
				continue
			}
			s.mu.Lock()
			idle := s.signIn == "" && s.status == StatusSignIn && !s.authorizing
			s.mu.Unlock()
			if !idle {
				continue // signed in since, already waiting, or another flow holds it
			}
			ctx, cancel := context.WithTimeout(context.Background(), signInTimeout)
			done := h.Busy("Signing in to "+name, cancel)
			aborted := false
			func() {
				// a panic must not strand the busy row or hold the context
				defer cancel()
				defer done()
				err = h.authorizeMCP(ctx, s)
				aborted = errors.Is(ctx.Err(), context.Canceled) // read before our own cancel lands
			}()
			if aborted {
				return // esc-esc stopped the queue; the failure report stays silent
			}
			if errors.Is(err, errAuthorizing) {
				continue // a row press or a second queue took it while we waited
			}
			if err != nil {
				failed = append(failed, name)
				continue
			}
			signed++
			h.signedInMCP(name)
			// gone, signed in or restarted under the queue: its news already showed
		}
		switch {
		case signed+len(failed) == 0:
			return
		case len(failed) == 0:
			h.Notify(slog.LevelInfo, fmt.Sprintf("Signed in to %d of %d", signed, len(failed)+signed), "")
		default:
			h.Notify(slog.LevelWarn, fmt.Sprintf("Signed in to %d of %d", signed, len(failed)+signed), strings.Join(failed, ", ")+" failed")
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
	if _, _, signedIn := auth.Ref().Get(); s.specSnapshot().URL == "" || !signedIn {
		return Usagef("%s is not signed in", name)
	}
	if !yes {
		return AskFirst("mcp sign-out "+name+" --yes", "", h.openMCP)
	}
	if err := auth.Logout(); err != nil {
		return err
	}
	// NOTE: a start is assumed to fail the same way, so none runs; a server
	// whose auth is optional waits for a sign-in until Restart. A disabled
	// server stays off: the grant is gone, but nothing was running anyway.
	enabled := s.enabledFlag()
	status := StatusSignIn
	if !enabled {
		status = StatusOff
	}
	out := &mcpServer{name: name, spec: s.specSnapshot(), status: status, enabled: enabled}
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
	if h.browser != nil {
		h.browser(u)
		return
	}
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return
	}
	h.GoSafe("open browser", func() { _ = proc.OpenURL(u) })
}
