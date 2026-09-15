// subagent.go runs subagents (DESIGN §6.3, M5): agent definitions, the
// model's agent tool, and the headless runs that zemi.agent.run and
// workflows share. A subagent is a fresh runner with its own history and a
// restricted tool set; its tool calls pass the same permission pipeline as
// the main agent's, and only its final message returns to the caller.
package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/goccy/go-yaml"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/skills"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// AgentDef is one subagent shape: builtin, or a markdown file under
// <config>/agents or <project>/.zemi/agents (frontmatter + system prompt).
type AgentDef struct {
	Name        string
	Description string
	Model       string   // "" inherits the session model
	Tools       []string // nil = every tool a subagent may use
	System      string
	Source      string // builtin, user or project
}

// subagentExcluded never reaches a subagent: nesting, the parent session's
// todo list, and asking the user (questions, plan approval).
var subagentExcluded = map[string]bool{"agent": true, "workflow": true, "todos": true, "question": true, "plan": true}

var builtinAgents = []AgentDef{
	{
		Name:        "explore",
		Description: "Fast read-only agent for finding code: files by pattern, symbols, how something works. It cannot edit files or run commands.",
		Tools:       []string{"read", "glob", "grep", "fetch", "sourcegraph", "memory_search", "skill"},
		System:      "You are a read-only search agent. Search broadly, read what matters, never guess. Your report goes to another agent, not the user: lead with the answer, cite paths relative to the project root with line numbers, and quote code only when the exact text matters.",
		Source:      "builtin",
	},
	{
		Name:        "general",
		Description: "General-purpose agent for research and multi-step tasks: searching code, reading files, running commands and making edits.",
		System:      "Complete the task with the tools. Read before changing, prefer read, grep, glob and edit over bash, and never run a server, watcher or interactive program in bash (use job_start). Then report to the agent that sent you what you found or changed, with paths and line numbers, and anything you could not verify.",
		Source:      "builtin",
	},
}

const maxAgentFileBytes = 1 << 20

// agentDefs lists the available definitions. On a name clash the higher
// trust source wins: user over builtin over project. Project agents load
// only once the project config tree is trusted.
func (h *Host) agentDefs() []AgentDef {
	var out []AgentDef
	seen := map[string]bool{}
	add := func(defs []AgentDef) {
		for _, d := range defs {
			if !seen[d.Name] {
				seen[d.Name] = true
				out = append(out, d)
			}
		}
	}
	if h.Dirs.ConfigHome != "" {
		add(h.loadAgentDir(filepath.Join(h.Dirs.ConfigHome, "agents"), "user"))
	}
	add(builtinAgents)
	if pdir := h.Dirs.ProjectConfigDir(); pdir != "" && h.projectTrusted.Load() {
		add(h.loadAgentDir(filepath.Join(pdir, "agents"), "project"))
	}
	return out
}

func (h *Host) agentDef(name string) (AgentDef, error) {
	if name == "" {
		name = "general"
	}
	var names []string
	for _, d := range h.agentDefs() {
		if d.Name == name {
			return d, nil
		}
		names = append(names, d.Name)
	}
	if pdir := h.Dirs.ProjectConfigDir(); pdir != "" && !h.projectTrusted.Load() {
		if _, err := os.Stat(filepath.Join(pdir, "agents", name+".md")); err == nil {
			return AgentDef{}, fmt.Errorf("agent %q is in %s, which is not trusted yet: the user runs /reload and trusts it", name, pdir)
		}
	}
	return AgentDef{}, fmt.Errorf("unknown agent %q (have %s)", name, strings.Join(names, ", "))
}

// loadAgentDir parses every *.md in dir; a broken file is logged and
// skipped so it cannot hide the others.
func (h *Host) loadAgentDir(dir, source string) []AgentDef {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []AgentDef
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		d, err := parseAgentFile(filepath.Join(dir, e.Name()), source)
		if err != nil {
			h.Log.Warn("agents", err.Error(), nil)
			continue
		}
		out = append(out, d)
	}
	return out
}

func parseAgentFile(path, source string) (AgentDef, error) {
	block, body, err := readFrontmatterFile(path)
	if err != nil {
		return AgentDef{}, err
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Model       string `yaml:"model"`
		Tools       any    `yaml:"tools"`
	}
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return AgentDef{}, fmt.Errorf("%s: frontmatter: %w", path, err)
	}
	d := AgentDef{
		Name:        fm.Name,
		Description: strings.TrimSpace(fm.Description),
		Model:       fm.Model,
		System:      strings.TrimSpace(body),
		Source:      source,
	}
	if d.Name == "" {
		d.Name = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	if d.Model == "inherit" {
		d.Model = ""
	}
	if !skills.ValidName(d.Name) {
		return AgentDef{}, fmt.Errorf("%s: name %q must match [a-z0-9][a-z0-9-]*", path, d.Name)
	}
	if d.Description == "" {
		return AgentDef{}, fmt.Errorf("%s: description is required", path)
	}
	// tools: "read, grep" or a yaml list
	switch t := fm.Tools.(type) {
	case string:
		for _, n := range strings.Split(t, ",") {
			if n = strings.TrimSpace(n); n != "" {
				d.Tools = append(d.Tools, n)
			}
		}
	case []any:
		for _, n := range t {
			if s, ok := n.(string); ok && s != "" {
				d.Tools = append(d.Tools, s)
			}
		}
	}
	return d, nil
}

// readRegularFile reads a config file, refusing symlinks and oversized files.
func readRegularFile(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s: not a regular file", path)
	}
	if fi.Size() > maxAgentFileBytes {
		return "", fmt.Errorf("%s: exceeds %d bytes", path, maxAgentFileBytes)
	}
	data, err := os.ReadFile(path)
	return string(data), err
}

// readFrontmatterFile reads a markdown file with an optional leading
// `---` yaml block (symlinks and oversized files refused).
func readFrontmatterFile(path string) (block, body string, err error) {
	data, err := readRegularFile(path)
	if err != nil {
		return "", "", err
	}
	s := strings.ReplaceAll(data, "\r\n", "\n")
	rest, ok := strings.CutPrefix(s, "---\n")
	if !ok {
		return "", s, nil
	}
	block, body, ok = strings.Cut(rest, "\n---")
	if !ok {
		return "", "", fmt.Errorf("%s: unterminated frontmatter", path)
	}
	_, body, _ = strings.Cut(body, "\n")
	return block, body, nil
}

// runSubagent runs def on prompt to completion as a tracked run (/agents)
// and returns its final message; progress receives live lines for the
// caller's UI row.
func (h *Host) runSubagent(ctx context.Context, def AgentDef, description, prompt string, progress func(string)) (string, error) {
	run, ctx := h.newRun(ctx, def, description, prompt, false)
	_ = h.tasks.add(run) // only background runs are capped
	return h.runAgent(ctx, run, def, prompt, progress)
}

// runAgent runs a registered run, recorded as a child session of the
// session that started it.
func (h *Host) runAgent(ctx context.Context, run *agentRun, def AgentDef, prompt string, progress func(string)) (text string, err error) {
	defer func() {
		run.finish(err)
		run.cancel()
		h.refresh()
	}()
	h.refresh()
	prof := &Profile{Model: def.Model}
	model, err := h.resolveModel(prof)
	if err != nil {
		return "", err
	}
	pipe := h.PipelineFor()
	pipe.TodosRead, pipe.TodosWrite, pipe.Question, pipe.ApprovePlan = nil, nil, nil, nil
	pipe.Seen = &sync.Map{} // what the main agent read says nothing about what this run saw
	if ask := pipe.Ask; ask != nil {
		pipe.Ask = func(req tools.Request) (bool, string) {
			req.Reason = strings.TrimSpace("agent " + def.Name + ": " + req.Reason)
			return ask(req)
		}
	}
	all := append(append(tools.All(pipe), h.luaFantasyTools()...), h.mcpFantasyTools(pipe)...)
	runner := agent.NewRunner(model, filterTools(all, def.Tools, subagentExcluded), h.systemWith(h.subagentSystem(def), nil))
	runner.ProviderOptions = h.Providers.EffortOptions(h.currentModelID(prof), h.Effort())
	runner.MaxOutputTokens = h.maxOutput(h.currentModelID(prof))

	sid := ""
	if h.DB != nil {
		// A run outside any session (Lua at idle) still gets a parent, so
		// /sessions never lists it as a top-level session.
		parent := run.parent
		if parent == "" {
			parent = "lua"
		}
		sid = store.NewID()
		if err := h.DB.CreateSession(sid, parent, truncRunes(run.description, 60), def.Name); err != nil {
			h.Log.Warn("agents", "subagent session not recorded", map[string]any{"error": err.Error()})
			sid = ""
		}
		run.mu.Lock()
		run.sessionID = sid
		run.mu.Unlock()
	}
	hooks := &subagentHooks{h: h, def: def, run: run, modelID: h.currentModelID(prof), progress: progress, sid: sid}
	runner.Hooks = hooks
	if sid != "" {
		runner.Persist = func(role string, msg fantasy.Message) error {
			parts, err := json.Marshal(msg)
			if err != nil {
				return err
			}
			return h.DB.AddMessage(&store.Message{SessionID: sid, Role: role, PartsJSON: string(parts)})
		}
	}
	h.EmitAsync("agent.subagent.start", map[string]any{"agent": def.Name, "description": run.description, "session": sid})
	err = runner.Send(ctx, prompt)
	h.EmitAsync("agent.subagent.finish", map[string]any{"agent": def.Name, "session": sid, "error": errString(err)})
	// history keeps the steps finished before a failure: text is then the
	// last thing the agent said, for callers that show a partial report
	text = lastAssistantText(runner.History)
	if err != nil {
		return text, err
	}
	if text == "" {
		text = "(the agent finished without a reply)"
	}
	return text, nil
}

func (h *Host) subagentSystem(def AgentDef) string {
	return "You are " + def.Name + ", a subagent of zemi working in " + h.Dirs.ProjectDir + "." +
		" The caller cannot see your work, only your final message: end with a complete," +
		" concise report of the facts it needs, with file paths where they matter.\n\n" + def.System
}

func lastAssistantText(history []fantasy.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == fantasy.MessageRoleAssistant {
			if s := strings.TrimSpace(concatText(history[i].Content)); s != "" {
				return s
			}
		}
	}
	return ""
}

// subagentHooks turns a subagent's stream into its run transcript and
// progress lines, and adds its token usage to the session tally (not to the
// context estimate).
type subagentHooks struct {
	h        *Host
	def      AgentDef
	run      *agentRun
	modelID  string
	progress func(string)
	sid      string
	stepText int // len(run.text) when the step began, where a retry restarts
}

func (s *subagentHooks) report(line string) {
	if s.progress != nil {
		s.progress(line)
	}
}

func (s *subagentHooks) TextDelta(text string) {
	s.run.mu.Lock()
	s.run.text += text
	s.run.mu.Unlock()
	s.h.refreshViewed(s.run)
}

func (s *subagentHooks) ReasoningDelta(string) {}

func (s *subagentHooks) ToolCallStart(id, name, input string) {
	run := s.run
	run.mu.Lock()
	run.flush()
	run.calls++
	n := run.calls
	run.rows[id] = len(run.tr)
	run.tr = append(run.tr, ChatMessage{Role: "assistant", Kind: "tool", Tool: name, Text: toolSummary(input), Label: toolLabel(input), Status: "run"})
	run.mu.Unlock()
	// one line: the widget wraps, and a long command would push the chat around
	s.report(fmt.Sprintf("%s · %d tool %s · %s %s", s.def.Name, n, plural(n, "call", "calls"), name,
		truncRunes(strings.Join(strings.Fields(toolSummary(input)), " "), 60)))
	s.h.refreshViewed(run)
}

func (s *subagentHooks) ToolCallEnd(id, _ string, ok bool, out string) {
	run := s.run
	run.mu.Lock()
	if i, found := run.rows[id]; found {
		run.tr[i].Status = map[bool]string{true: "ok", false: "err"}[ok]
		run.tr[i].Output = outputTail(out)
	}
	run.mu.Unlock()
	s.h.refreshViewed(run)
}

func (s *subagentHooks) StepStart() {
	s.run.mu.Lock()
	s.stepText = len(s.run.text)
	s.run.mu.Unlock()
}

func (s *subagentHooks) Retrying(err error, delay time.Duration) {
	s.run.mu.Lock()
	s.run.text = s.run.text[:min(s.stepText, len(s.run.text))]
	s.run.mu.Unlock()
	s.report(fmt.Sprintf("%s · retrying in %s", s.def.Name, delay.Round(time.Second)))
}

func (s *subagentHooks) TurnDone(err error, usage fantasy.Usage) {
	h := s.h
	cost := h.turnCost(s.modelID, usage)
	ag := h.agents
	ag.mu.Lock()
	if ag.sessionID == s.run.parent { // a background run may outlive its session
		ag.sessIn += usage.InputTokens
		ag.sessOut += usage.OutputTokens
		ag.sessCost += cost
	}
	ag.mu.Unlock()
	if h.DB != nil {
		for _, id := range []string{s.sid, s.run.parent} {
			if id != "" {
				_ = h.DB.AddUsage(id, usage.InputTokens, usage.OutputTokens, cost)
			}
		}
	}
	_, n, _, _ := s.run.state()
	s.report(fmt.Sprintf("%s · %d tool %s", s.def.Name, n, plural(n, "call", "calls")))
	h.refresh()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// filterTools keeps the tools named in want (nil keeps all) minus skip.
func filterTools(all []fantasy.AgentTool, want []string, skip map[string]bool) []fantasy.AgentTool {
	keep := map[string]bool{}
	for _, n := range want {
		keep[n] = true
	}
	var out []fantasy.AgentTool
	for _, t := range all {
		name := t.Info().Name
		if skip[name] || (want != nil && !keep[name]) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// setToolDetail updates the live progress line under a running tool row.
func (h *Host) setToolDetail(callID, detail string) {
	ag := h.agents
	ag.mu.Lock()
	for i := range ag.tools {
		if ag.tools[i].ID == callID {
			ag.tools[i].Detail = detail
		}
	}
	ag.mu.Unlock()
	h.agentEvent(AgentEvent{Kind: "tool_progress", CallID: callID})
}

// ---------------------------------------------------------------------------
// the agent tool

// agentTool is the model's entry point to subagents. Parallel: several
// calls in one step run concurrently.
type agentTool struct {
	h    *Host
	defs []AgentDef
}

func (h *Host) agentTool() fantasy.AgentTool { return &agentTool{h: h, defs: h.agentDefs()} }

func (t *agentTool) Info() fantasy.ToolInfo {
	var b strings.Builder
	b.WriteString("Launch a subagent for a self-contained task. It works with its own context and tools and returns one final report." +
		" Use it for broad searches and research that would flood your context, and for independent tasks in parallel:" +
		" call the tool several times in one message. Prefer explore for questions about code; it only reads, so it never" +
		" stops to ask the user for permission. The subagent cannot see this conversation: the prompt must carry" +
		" everything it needs and say what to report back. Its report is not shown to the user; relay what matters." +
		" Set background for long work you do not need to wait on: the tool returns at once and the report arrives later" +
		" as a message, so keep working (or end your turn) meanwhile.\n\nAgents:")
	names := make([]string, 0, len(t.defs))
	for _, d := range t.defs {
		fmt.Fprintf(&b, "\n- %s: %s", d.Name, truncRunes(d.Description, 200))
		names = append(names, d.Name)
	}
	return fantasy.ToolInfo{
		Name:        "agent",
		Description: b.String(),
		Parameters: map[string]any{
			"description": map[string]any{"type": "string", "description": "3-5 word label shown to the user"},
			"prompt":      map[string]any{"type": "string", "description": "the full task"},
			"agent":       map[string]any{"type": "string", "enum": names, "description": "agent to run: explore for reading and searching, general only when it must run commands or edit"},
			"background":  map[string]any{"type": "boolean", "description": "run detached; the report arrives later as a message"},
		},
		Required: []string{"description", "prompt", "agent"},
		Parallel: true,
	}
}

func (t *agentTool) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (t *agentTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (t *agentTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var in struct {
		Description string `json:"description"`
		Prompt      string `json:"prompt"`
		Agent       string `json:"agent"`
		Background  bool   `json:"background"`
	}
	if err := json.Unmarshal([]byte(call.Input), &in); err != nil {
		return fantasy.NewTextErrorResponse("invalid tool input: " + err.Error()), nil
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return fantasy.NewTextErrorResponse("prompt is required"), nil
	}
	if resp, stop, err := t.h.preCheck("agent", call.Input); stop {
		return resp, err
	}
	def, err := t.h.agentDef(in.Agent)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if in.Background {
		id, err := t.h.startBackgroundAgent(def, in.Description, in.Prompt)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		t.h.setToolDetail(call.ID, def.Name+" · background "+id+" (/agents)")
		return fantasy.NewTextResponse("Started background agent " + id + ". Its report will arrive as a message when it finishes; do not wait for it."), nil
	}
	start := time.Now()
	var last atomic.Value
	text, err := t.h.runSubagent(ctx, def, in.Description, in.Prompt, func(line string) {
		last.Store(line)
		t.h.setToolDetail(call.ID, line)
	})
	summary, _ := last.Load().(string)
	if summary == "" {
		summary = def.Name
	}
	t.h.setToolDetail(call.ID, summary+" · "+fmtDuration(time.Since(start)))
	if err != nil {
		msg := "agent failed: " + friendlyError(err)
		if errors.Is(err, context.Canceled) {
			msg = "agent cancelled"
		}
		if text != "" {
			msg += "\n\nIts last message before that:\n" + text
		}
		return fantasy.NewTextErrorResponse(msg), nil
	}
	return fantasy.NewTextResponse(truncOutput(text)), nil
}

// preCheck runs the tool.pre events for a host-implemented tool: deny
// becomes an error result, halt stops the turn. Rewrites are not offered.
func (h *Host) preCheck(tool, input string) (fantasy.ToolResponse, bool, error) {
	var m map[string]any
	_ = json.Unmarshal([]byte(input), &m)
	decision, reason, _, _ := h.toolPreHook()(tool, m)
	switch decision {
	case "deny":
		return fantasy.NewTextErrorResponse("denied by hook: " + reason), true, nil
	case "halt":
		return fantasy.ToolResponse{}, true, fmt.Errorf("halted by hook: %s", reason)
	}
	return fantasy.ToolResponse{}, false, nil
}

func fmtDuration(d time.Duration) string {
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm %02ds", s/60, s%60)
}

// ---------------------------------------------------------------------------
// /agents and zemi.agent.run

// AgentsCommandsStep registers /agents.
func (h *Host) AgentsCommandsStep() lua.BootStep {
	return lua.BootStep{
		Name: "agents commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			_, err := h.Commands.Add(&Command{
				Name: "agents", NArgs: "*", Desc: "agents at work and the ones zemi can launch",
				GoFn: func(h *Host, args string) error {
					sub, id, _ := strings.Cut(strings.TrimSpace(args), " ")
					switch id = strings.TrimSpace(id); sub {
					case "":
						return h.openAgents()
					case "view":
						return h.ViewAgent(id)
					case "stop":
						return h.stopRun(id)
					}
					return fmt.Errorf("unknown /agents subcommand %q (view <id>, stop [id])", sub)
				},
				Origin: lua.OriginBuiltin, Gen: Gen(g.ID),
			})
			return err
		},
	}
}

func (h *Host) openAgents() error {
	ui := h.hooks()
	if ui == nil {
		return errors.New("no UI")
	}
	defs := h.agentDefs()
	sort.SliceStable(defs, func(i, j int) bool { return defs[i].Source < defs[j].Source })
	items := h.agentRunItems()
	if len(items) > 0 {
		items = append(items, map[string]any{"text": ""})
	}
	for _, d := range defs {
		items = append(items, map[string]any{"text": fmt.Sprintf("%-12s %-8s %s", d.Name, d.Source, truncRunes(d.Description, 80))})
	}
	ui.OpenOverlay(Overlay{ID: "agents", Title: "Agents (" + filepath.Join("~/.config/zemi", "agents") + ", .zemi/agents)", Origin: lua.OriginBuiltin, Items: items})
	return nil
}

// agentRunFns adds zemi.agent.run and zemi.agent.list to the agent
// namespace. run is async: on_done(result, err) fires on the interpreter
// once the subagent finishes; the returned handle cancels it.
func (h *Host) agentRunFns(L *glua.LState, tb *glua.LTable) {
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		var rows []any
		for _, d := range h.agentDefs() {
			rows = append(rows, map[string]any{"name": d.Name, "description": d.Description, "source": d.Source, "model": d.Model})
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))
	tb.RawSetString("run", L.NewFunction(func(L *glua.LState) int {
		spec := L.CheckTable(1)
		onDone, _ := L.Get(2).(*glua.LFunction)
		// agents act with the user's tools and permissions: only user config
		// (and shipped code) may start them from Lua
		if o := h.RT.CurrentOrigin(); o != lua.OriginUser && o != lua.OriginBuiltin {
			L.RaiseError("zemi.agent.run: only user config may launch agents")
			return 0
		}
		prompt := rawString(spec, "prompt")
		if strings.TrimSpace(prompt) == "" {
			L.RaiseError("zemi.agent.run: prompt is required")
			return 0
		}
		def, err := h.agentDef(rawString(spec, "agent"))
		if err != nil {
			L.RaiseError("zemi.agent.run: %v", err)
			return 0
		}
		desc := rawString(spec, "description")
		if desc == "" {
			desc = truncRunes(prompt, 40)
		}
		ctx, cancel := context.WithCancel(context.Background())
		gen := h.currentGen()
		h.GoSafe("zemi.agent.run", func() {
			defer cancel()
			text, err := h.runSubagent(ctx, def, desc, prompt, nil)
			switch {
			case onDone == nil:
			case err != nil:
				h.callLuaAsync(gen, onDone, nil, friendlyError(err))
			default:
				h.callLuaAsync(gen, onDone, text, nil)
			}
		})
		handle := L.NewTable()
		handle.RawSetString("cancel", L.NewFunction(func(L *glua.LState) int {
			cancel()
			return 0
		}))
		L.Push(handle)
		return 1
	}))
}
