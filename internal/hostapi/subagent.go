// subagent.go runs subagents (DESIGN §6.3): agent definitions, the
// model's agent tool, and the detached runs that zemi.agent.run and
// workflows share. A subagent is a fresh runner with its own history and a
// restricted tool set; its tool calls pass the same permission pipeline as
// the main agent's, and only its final message returns to the caller.

package hostapi

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/goccy/go-yaml"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/plugin"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/set"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// AgentDef is one subagent shape: builtin, or a markdown file under
// <config>/agents, a plugin's agents or <repo>/.agents/agents (frontmatter +
// system prompt).
type AgentDef struct {
	Name        string
	Description string
	// Model is a target name or a provider/model id; "" takes models.agents.
	Model  string
	Tools  []toolRef // nil = every tool a subagent may use
	System string
	Source lua.Origin
	Path   string // the file, "" for a builtin
	// ReadOnly is file writes denied even when a tool could make one.
	ReadOnly bool
}

// toolRef is one entry of a definition's tools list, the "-name" prefix of
// the agent file parsed at the read: name is the tool, deny that the entry
// removes it.
type toolRef struct {
	name string
	deny bool
}

// subagentExcluded never reaches a subagent: nesting, the parent session's
// todo list, the user's attention (questions, plan approval) and the main
// agent's lua tool. zoom and date stay: the run opens on the view, and they
// are how it reads the chat behind it. Listing one in an agent file drops it.
var subagentExcluded = set.Of(tools.ToolAgent, tools.ToolWorkflow, tools.ToolTodos, tools.ToolQuestion, tools.ToolPlan, tools.ToolLua)

// forkAgent is the run that continues the caller's conversation instead of
// starting fresh: not a definition but the same agent carried on, so only its
// name and description below are read.
const forkAgent = "fork"

// readOnlyTools is the set every read-only builtin agent gets: it reads,
// searches and loads skills, and changes nothing.
var readOnlyTools = []toolRef{
	{name: string(tools.ToolRead)},
	{name: string(tools.ToolGlob)},
	{name: string(tools.ToolGrep)},
	{name: string(tools.ToolSkill)},
}

var builtinAgents = []AgentDef{
	{
		Name:        forkAgent,
		Description: "Continues this conversation instead of starting fresh, so it needs no briefing.",
		Source:      lua.OriginBuiltin,
	},
	{
		Name:        "explore",
		Description: "Read-only: finds files, symbols and how something works. Cannot edit or run commands.",
		Tools:       readOnlyTools,
		ReadOnly:    true,
		System:      "Search broadly, read what matters, never guess. Lead with the answer, quote code only when the exact text matters, and when you did not find it, say where you looked.",
		Source:      lua.OriginBuiltin,
	},
	{
		Name:        "general",
		Description: "Multi-step work: searches, reads, runs commands and edits.",
		// this replaces basePrompt rather than extending it, so the git and
		// verification rules have to be said again here
		System: "Carry the task through: change, verify with the narrowest test or build that covers the change, report. Do only what the task asks; do not commit, push or change git state unless it says to.",
		Source: lua.OriginBuiltin,
	},
	{
		// a read-only architect persona whose report is a concrete plan draft
		// the caller presents with its plan tool.
		Name:        "plan",
		Description: "Read-only: explores the code and drafts an implementation plan with a Critical Files section, for a large or unclear change.",
		Tools:       readOnlyTools,
		ReadOnly:    true,
		System:      "Read the code the change touches and the patterns it should reuse; never guess what a read would answer. Then report a plan someone could implement without guessing: the goal, what changes where (files, and code to reuse as path:line), how to verify, the assumptions, and only the approach you recommend. End with a Critical Files section naming the 3-5 files that matter most, then the open questions only the user can answer.",
		Source:      lua.OriginBuiltin,
	},
}

const maxAgentFileBytes = 1 << 20

// agentDefs lists the available definitions. On a name clash the higher
// trust source wins: user over builtin over plugin over project. Project
// agents load only once the project tree is trusted.
func (h *Host) agentDefs() []AgentDef {
	defs := slices.Clone(builtinAgents)
	h.layerFiles("agents", ".md", func(f layerFile) bool {
		if f.name == forkAgent {
			h.RecordErrorOnce("agents", "", f.path+" is ignored: fork continues the caller's conversation, so there is no definition to write")
			return false
		}
		d, err := h.parseAgentFile(f.name, f.path, f.origin, f.prefix != "")
		if err != nil {
			h.RecordErrorOnce("agents", "", err.Error())
			return false
		}
		defs = append(defs, d)
		return true
	})
	sortBySource(defs, func(d AgentDef) lua.Origin { return d.Source })
	return dedupeByName(defs, func(d AgentDef) string { return d.Name }, nil)
}

func (h *Host) projectAgentDir() string {
	if pdir := h.Dirs.ProjectConfigDir(); pdir != "" {
		return filepath.Join(pdir, "agents")
	}
	return ""
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
	if dir := h.projectAgentDir(); dir != "" && !h.ProjectTrusted() {
		if _, err := os.Stat(filepath.Join(dir, name+".md")); err == nil {
			return AgentDef{}, fmt.Errorf("%s", h.untrustedNote("agent", name, dir))
		}
	}
	return AgentDef{}, fmt.Errorf("unknown agent %q (have %s)", name, strings.Join(names, ", "))
}

// zemiToolNames is every tool name an agent definition may keep: the
// builtins. A Claude agent naming anything else loses the entry, since it
// would fail the invocation filter.
var zemiToolNames = set.Of(
	tools.ToolShell, tools.ToolRead, tools.ToolWrite,
	tools.ToolEdit, tools.ToolGlob, tools.ToolGrep,
	tools.ToolTodos,
	tools.ToolQuestion, tools.ToolPlan, tools.ToolSkill,
	tools.ToolJobStart, tools.ToolJobRead, tools.ToolJobList,
	tools.ToolJobKill, tools.ToolAgent, tools.ToolWorkflow,
	tools.ToolLua, tools.ToolUpdate, tools.ToolSearch,
)

// agentIgnoredKeys are the Claude agent frontmatter keys zemi has no use for.
var agentIgnoredKeys = []string{"skills", "color", "background", "memory", "isolation", "effort", "maxTurns", "permissionMode", "hooks", "mcpServers"}

// parseAgentFile parses one agent definition. For a Claude-layout plugin's
// (claude), the tools are Claude names through the alias map and anything
// zemi does not have is dropped with a word, the model must stay one zemi
// can resolve, and the keys zemi has no use for are named once.
func (h *Host) parseAgentFile(name, path string, source lua.Origin, claude bool) (AgentDef, error) {
	block, body, err := readFrontmatterFile(path)
	if err != nil {
		return AgentDef{}, err
	}
	var fm struct {
		Description     string `yaml:"description"`
		WhenToUse       string `yaml:"when-to-use"`
		WhenToUseSnake  string `yaml:"when_to_use"`
		Model           string `yaml:"model"`
		Tools           any    `yaml:"tools"`
		DisallowedTools any    `yaml:"disallowedTools"`
		ReadOnly        bool   `yaml:"read_only"`
	}
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return AgentDef{}, fmt.Errorf("%s: frontmatter: %w", path, err)
	}
	d := AgentDef{
		Name:   name,
		Model:  fm.Model,
		System: strings.TrimSpace(body),
		Source: source,
		Path:   path,
		// the trigger may be written under Claude's name for it
		Description: cmp.Or(strings.TrimSpace(fm.Description), strings.TrimSpace(fm.WhenToUse), strings.TrimSpace(fm.WhenToUseSnake)),
		ReadOnly:    fm.ReadOnly,
	}
	if claude {
		if root := h.claudeRootOf(path); root != "" {
			d.System = plugin.ExpandRootData(d.System, root, h.pluginDataDir(h.PluginOf(root)))
		}
		if strings.Contains(d.System, "!`") {
			h.Log.Warn("agent body has !`cmd` shell blocks, they stay literal text", "origin", "agents", "path", path)
		}
		switch fm.Model {
		case "", "inherit":
		default:
			// an id that would fail checkModelID at every run is dropped here
			h.Log.Warn(path+": model is not one zemi knows, ignored", "origin", "agents", "model", fm.Model)
			d.Model = ""
		}
	}
	if d.Model == "inherit" {
		d.Model = ""
	}
	if d.Description == "" {
		return AgentDef{}, fmt.Errorf("%s: description is required", path)
	}
	// tools: "read, grep" or a yaml list; disallowedTools joins as -name
	var names []string
	toolList := func(v any) {
		switch t := v.(type) {
		case string:
			for n := range strings.SplitSeq(t, ",") {
				if n = strings.TrimSpace(n); n != "" {
					names = append(names, n)
				}
			}
		case []any:
			for _, n := range t {
				if s, ok := n.(string); ok && s != "" {
					names = append(names, s)
				}
			}
		}
	}
	toolList(fm.Tools)
	toolList(fm.DisallowedTools)
	var dropped []string
	for _, n := range names {
		deny := strings.HasPrefix(n, "-")
		tool := strings.TrimPrefix(n, "-")
		if claude {
			if mapped := claudeTool(tool); zemiToolNames.Has(mapped) {
				tool = string(mapped)
			} else {
				if !deny { // a negation of what zemi does not have is safe to drop
					dropped = append(dropped, n)
				}
				continue
			}
		}
		d.Tools = append(d.Tools, toolRef{name: tool, deny: deny})
	}
	if len(dropped) > 0 {
		h.Log.Warn(path+": tools zemi does not have, dropped", "origin", "agents", "tools", strings.Join(dropped, ", "))
	}
	if claude {
		var raw map[string]any
		if yaml.Unmarshal([]byte(block), &raw) == nil {
			var ignored []string
			for _, k := range agentIgnoredKeys {
				if _, ok := raw[k]; ok {
					ignored = append(ignored, k)
				}
			}
			if ignored != nil {
				h.Log.Warn(path+": frontmatter zemi does not use, ignored", "origin", "agents", "keys", strings.Join(ignored, ", "))
			}
		}
	}
	return d, nil
}

// runSubagent runs def on prompt to completion as a tracked run (/agents)
// and returns its id and final message.
func (h *Host) runSubagent(ctx context.Context, def AgentDef, description, prompt string) (string, string, error) {
	run, ctx := h.newRun(ctx, def, description, prompt, false)
	_ = h.tasks.add(run) // only background runs are capped
	text, err := h.runAgent(ctx, run, def, prompt, nil)
	return run.id, text, err
}

// runSessionKey carries a subagent run's session id on its tools' ctx; the
// main agent's calls belong to the live session.
type runSessionKey struct{}

// runKey carries the run itself, so a tool of that run can reach its caller
// (the update tool).
type runKey struct{}

// runOf is the subagent run a tool call belongs to, nil for the main agent's.
func runOf(ctx context.Context) *agentRun {
	run, _ := ctx.Value(runKey{}).(*agentRun)
	return run
}

// runAgent runs a registered run, recorded as a child session of the
// session that started it.
func (h *Host) runAgent(ctx context.Context, run *agentRun, def AgentDef, prompt string, progress func(string)) (text string, err error) {
	defer func() {
		if err != nil {
			h.Log.Warn("subagent run failed", "origin", "agents", "agent", def.Name, "run", run.id, "error", errString(err))
		} else {
			h.Log.Info("subagent run done", "origin", "agents", "agent", def.Name, "run", run.id)
		}
		run.finish(h, err)
		run.cancel()
		h.refresh()
	}()
	h.refresh()
	h.watchAgents() // an open /agents follows the run from here
	run.mu.Lock()
	prev := run.runner
	// fresh per attempt: a continuation makes the run live again, and a
	// waiter must follow the attempt it started on
	run.end = make(chan struct{})
	run.retries, run.retryAt, run.retryWhy = 0, time.Time{}, ""
	run.mu.Unlock()
	setup, err := h.runSetup(ctx, run, prev, def, prompt)
	if err != nil {
		return "", err
	}
	runner, modelID, pipe := setup.runner, setup.modelID, setup.pipe
	h.Log.Info("subagent run started", "origin", "agents", "agent", def.Name, "run", run.id,
		"parent", run.parent, "prompt", clipLine(sanitize.String(prompt), 120))

	run.mu.Lock()
	sid, calls := run.sessionID, run.calls
	run.runner = runner
	run.mu.Unlock()
	var rows []*store.Message
	if sid != "" {
		// a continuation may edit what the run read before
		if rows, err = h.DB.Messages(sid); err != nil {
			return "", err
		}
		h.seedReadsInto(pipe.Seen, rows)
	}
	switch {
	case prev != nil:
		// the history the run ended with (Send closes any dangling call),
		// on the same child session
		runner.Continue(prev)
	case sid != "":
		// a run this process forgot: its rows are the history
		runner.History, _ = historyFrom(rows)
	case h.DB != nil:
		// the run id is the session id, so send_to finds the rows of a run
		// the process no longer holds (a resume, or one past maxKeptRuns)
		sid = run.id
		if err := h.DB.CreateSession(sid, cmp.Or(run.parent, "lua"), run.description, def.Name); err != nil {
			h.Log.Warn("subagent session not recorded", "origin", "agents", "error", err)
			sid = ""
		}
		run.mu.Lock()
		run.sessionID = sid
		run.mu.Unlock()
	}
	// a fork keeps the caller's session: that is what the conversation is cached under
	cache := cmp.Or(setup.cache, sid)
	runner.ProviderOptions, runner.Headers = h.Providers.CallOptions(agent.ModelID(modelID), h.Effort(modelID), cache), agent.SessionHeaders(cache)
	seedLoaded(setup.loaded, runner.History)
	ctx = context.WithValue(ctx, runSessionKey{}, sid)
	ctx = context.WithValue(ctx, runKey{}, run)
	hooks := &subagentHooks{h: h, def: def, run: run, modelID: modelID, progress: progress, sid: sid, ctx: ctx, base: calls}
	runner.Hooks = hooks
	if sid != "" {
		runner.Persist = func(msg fantasy.Message) error {
			run.mu.Lock()
			thought := run.thinkTime()
			run.mu.Unlock()
			_, err := h.storeMessage(sid, msg, thought)
			return err
		}
	}
	prompt = setup.prompt
	if run.forkBase == len(runner.History) && pipe.Pad != "" && setup.writes {
		prompt += "\n\n" + fmt.Sprintf(scratchOwn, filepath.Join(pipe.Pad, def.Name+"-"+run.id))
	}
	h.consultHooks(EventAgentSubagentStart, map[string]any{"agent": def.Name, "description": run.description})
	h.EmitAsync(EventAgentSubagentStart, map[string]any{"agent": def.Name, "description": run.description, "session": sid})
	runner.Steer = run.takeQueue
	// a fork reports only what it said, not the conversation it opened with
	own := run.forkBase
	own, err = h.sendPruning(ctx, runner, modelID, prompt, own)
	for next := run.followUp(err); next != ""; next = run.followUp(err) {
		own, err = h.sendPruning(ctx, runner, modelID, next, own)
	}
	run.forkBase = own
	said := func() []fantasy.Message { return runner.History[min(own, len(runner.History)):] }
	// NOTE: tools.Validate checks required fields and types, not every
	// keyword (enum, bounds); a deeper validator is the upgrade.
	var report string
	for tries := 0; err == nil && run.schema != nil; tries++ {
		var bad error
		if report, bad = schemaReport(run.schema, lastAssistantText(said())); bad == nil {
			break
		}
		if tries == 2 {
			err = fmt.Errorf("the report is not the JSON asked for: %w", bad)
			break
		}
		own, err = h.sendPruning(ctx, runner, modelID, "Your reply is not the JSON asked for: "+bad.Error()+". Reply with only the JSON object.", own)
	}
	h.consultHooks(EventAgentSubagentFinish, map[string]any{"agent": def.Name, "error": errString(err)})
	h.EmitAsync(EventAgentSubagentFinish, map[string]any{"agent": def.Name, "session": sid, "error": errString(err)})
	// history keeps the steps finished before a failure: text is then the
	// last thing the agent said, for callers that show a partial report
	text = lastAssistantText(said())
	if n := run.droppedCount(); n > 0 {
		text = strings.TrimSpace(fmt.Sprintf("%s\n\n(%d message(s) sent to it were never read: the run ended first)", text, n))
	}
	if err != nil {
		return text, err
	}
	if run.schema != nil {
		return report, nil
	}
	if text == "" {
		text = "(the agent finished without a reply)"
	}
	return text, nil
}

// forkNested is why a run cannot start a fork: the conversation it would
// continue is the one it already runs.
func forkNested(run *agentRun) string {
	if run.agent == forkAgent {
		return "this run is already a fork of the conversation: a fork cannot fork again, so do the work here and put what you cannot do in your report"
	}
	return "a fork continues the live conversation, and this call is a run of it: do the work here, or report what you cannot"
}

// runPipeline is the pipeline a run's tool calls go through: the live one, bound
// to the session the run belongs to (a background run outlives it) and stripped
// of what only the main agent may do.
func (h *Host) runPipeline(run *agentRun, name, modelID string) *tools.Pipeline {
	pipe := h.PipelineFor()
	pipe.TodosRead, pipe.TodosWrite, pipe.Question, pipe.ApprovePlan = nil, nil, nil, nil
	pipe.Images = func() bool { return h.takesImages(modelID) }
	if run.parent != "" {
		pipe.Pad = h.scratchFor(run.parent)
	}
	h.labelAsks(pipe, run, name)
	return pipe
}

// capRun caps a run's output and ends its turn when the window nears full:
// the run then drops the middle of its history once and goes on.
func (h *Host) capRun(runner *agent.Runner, modelID string) {
	runner.MaxOutputTokens = h.maxOutput(modelID)
	window := h.contextWindow(modelID)
	runner.Full = func(used int64) bool { return window > 0 && used >= window*3/4 }
}

// writesFiles reports tools that can change files: the run's task then names the
// pad directory it owns.
func writesFiles(kept []fantasy.AgentTool) bool {
	return hasTool(kept, tools.ToolShell) || hasTool(kept, tools.ToolWrite) || hasTool(kept, tools.ToolEdit)
}

// copyMap is a snapshot of m, for what a run inherits from the conversation and
// must keep its own.
func copyMap(m *sync.Map) *sync.Map {
	out := &sync.Map{}
	m.Range(func(k, v any) bool { out.Store(k, v); return true })
	return out
}

// runSetup is what a run's tail needs, however its runner was built.
type runSetup struct {
	runner  *agent.Runner
	pipe    *tools.Pipeline
	modelID string
	// loaded is the MCP tools this run offers: the names a step sends.
	loaded *sync.Map
	// cache is the session keying the run's calls, "" for its own child session.
	cache  string
	writes bool
	prompt string
}

// runSetup builds the runner a run sends on: a fresh subagent, or a fork that
// continues the caller's conversation.
func (h *Host) runSetup(ctx context.Context, run *agentRun, prev *agent.Runner, def AgentDef, prompt string) (*runSetup, error) {
	if def.Name == forkAgent {
		return h.forkSetup(ctx, run, prev, prompt)
	}
	return h.subagentSetup(run, def, prompt)
}

// subagentSetup builds a fresh run: its own prompt, its own tool set, its own
// pipeline over the same permission gate.
func (h *Host) subagentSetup(run *agentRun, def AgentDef, prompt string) (*runSetup, error) {
	modelID := h.subagentModelID(def)
	model, err := h.resolveModel(modelID)
	if err != nil {
		return nil, err
	}
	pipe := h.runPipeline(run, def.Name, modelID)
	pipe.Seen = &sync.Map{} // what the main agent read says nothing about what this run saw
	var sent sync.Map       // likewise the instruction files its reads carried
	pipe.Instructions = func(path string) string {
		return h.nearestInstructions(path, h.instructionFiles(), func(p string) bool {
			_, dup := sent.LoadOrStore(p, true)
			return dup
		})
	}
	// a run's own definition decides whether the project is read-only to it
	if def.ReadOnly {
		pipe.ReadOnly = func() bool { return true }
	}
	kept, unknown := childTools(h, pipe, def.Tools)
	if len(unknown) > 0 {
		return nil, fmt.Errorf("agent %s: no tool is named %s", def.Name, strings.Join(unknown, ", "))
	}
	loaded := &sync.Map{} // what the main agent loaded says nothing about what this run needs
	kept, active := h.deferMCP(kept, loaded)
	sys, ctxMsg := h.systemWith(h.subagentSystem(def, kept, def.ReadOnly), kept, pipe.Pad, def.ReadOnly)
	runner := agent.NewRunner(model, kept, sys)
	runner.Context, runner.ActiveTools = ctxMsg, active
	// the run opens on the view: what the caller's chat holds is what its
	// work builds on, read-only
	if msg, ok := h.viewMessage(); ok {
		runner.History = []fantasy.Message{msg}
	}
	run.forkBase = len(runner.History) // the run's own messages start past the view
	h.capRun(runner, modelID)
	return &runSetup{
		runner: runner, pipe: pipe, modelID: modelID, loaded: loaded, prompt: prompt,
		writes: writesFiles(kept),
	}, nil
}

// forkSetup builds the runner a fork sends on: the caller's own prompt, tools,
// model and request options over the conversation as the model last sent it, so
// the first call reads the cache of that request instead of writing its prefix.
func (h *Host) forkSetup(ctx context.Context, run *agentRun, prev *agent.Runner, prompt string) (*runSetup, error) {
	// a fork continues the live conversation, and a run of it has none of its own
	if run := runOf(ctx); run != nil {
		return nil, errors.New(forkNested(run))
	}
	ag := h.agents
	ag.mu.Lock()
	live, active := ag.runner, ag.active
	ag.mu.Unlock()
	from := cmp.Or(prev, live)
	switch {
	case prev == nil && run.sessionID != "":
		// its rows hold the fork's own work, never the conversation it opened with
		return nil, errors.New("zemi no longer holds this fork's conversation: continue it while it still does, or start a new fork")
	case from == nil || prev == nil && !active:
		return nil, errors.New("a fork continues the turn that is running: it needs a message already sent, and that turn still going")
	}
	modelID := h.sessionModelID()
	if prev != nil {
		modelID = cmp.Or(run.model, modelID)
	}
	infos := from.Tools()
	pipe := h.runPipeline(run, forkAgent, modelID)
	prompted := h.pinFiles()
	var sent sync.Map
	pipe.Instructions = func(path string) string {
		return h.nearestInstructions(path, prompted, func(p string) bool {
			_, dup := sent.LoadOrStore(p, true)
			return dup
		})
	}
	// the fork inherits the conversation's reads and MCP loads; what it reads and
	// loads itself stays its own, out of the caller's live maps
	reads, session := h.sessionLogs()
	loaded := copyMap(session)
	pipe.Seen = copyMap(reads)
	kept, offers, err := h.forkTools(pipe, infos, loaded)
	if err != nil {
		return nil, err
	}
	runner, err := from.Fork(kept)
	if err != nil {
		return nil, err
	}
	runner.ActiveTools = offers
	h.capRun(runner, modelID)
	if run.forkBase == 0 {
		run.forkBase = len(runner.History)
	}
	run.model = modelID
	return &runSetup{
		runner: runner, pipe: pipe, modelID: modelID, loaded: loaded, cache: run.parent,
		writes: writesFiles(kept),
		prompt: forkTask + "\n\n" + prompt,
	}, nil
}

// forkTools builds a fork's tool set over its own pipeline: the caller's tools,
// in its order, since any difference there is a prefix the provider writes
// instead of reading.
func (h *Host) forkTools(pipe *tools.Pipeline, infos []fantasy.ToolInfo, loaded *sync.Map) ([]fantasy.AgentTool, func() []string, error) {
	build := func(p *tools.Pipeline) ([]fantasy.AgentTool, []string) {
		all := h.toolsFor(p)
		all, _ = h.deferMCP(all, loaded)
		out, _ := pickTools(all, infos) // the run refused already if a name is gone
		return out, nil
	}
	all := h.toolsFor(pipe)
	wireLua(all, build)
	// the caller's own set decides what a step offers, deferral included: those
	// names are the prefix key, so this run's own next step would send another
	// array than the request it continues
	names := make([]string, len(infos))
	for i, info := range infos {
		names[i] = info.Name
	}
	all, _ = filterTools(all, names, nil)
	all, active := h.deferMCP(all, loaded)
	kept, err := pickTools(all, infos)
	return kept, active, err
}

// pickTools is a fork's tool array: the tools the caller sent, in its order and
// with its schemas. A name this run cannot build refuses the fork rather than
// sending another array, which would write the cache the fork exists to read.
func pickTools(all []fantasy.AgentTool, infos []fantasy.ToolInfo) ([]fantasy.AgentTool, error) {
	by := make(map[string]fantasy.AgentTool, len(all))
	for _, t := range all {
		by[t.Info().Name] = t
	}
	out := make([]fantasy.AgentTool, len(infos))
	for i, info := range infos {
		t, ok := by[info.Name]
		if !ok {
			return nil, fmt.Errorf("no tool is named %s any more: the tool set changed while this turn ran, so a fork cannot send the caller's own request", info.Name)
		}
		out[i] = pinnedTool{AgentTool: t, info: info}
	}
	return out, nil
}

// labelAsks names the run behind its tool calls, so the user reads who asks.
func (h *Host) labelAsks(pipe *tools.Pipeline, run *agentRun, name string) {
	ask := pipe.Ask
	if ask == nil {
		return
	}
	who := name + " agent"
	if f := h.tasks.flow(run.workflow); f != nil {
		who = "workflow " + f.id + " · " + who // its descriptions are the model's words
	}
	pipe.Ask = func(req tools.Request) (bool, string) {
		req.Agent = who
		return ask(req)
	}
}

// sendPruning runs one subagent turn inside the context window: a run that
// fills it drops the middle of its history, once - the task and a recent
// tail stay - and goes on. A subagent has no summarizer: its history is its
// own and only its report leaves the run. A cap the model refuses is dropped
// once. own is where the run's own messages start and moves with what a drop
// takes from the front.
// NOTE: the dropped steps are lost unsummarized, a follow-up message older
// than the tail among them, and a run that fills the window again ends with
// the error.
func (h *Host) sendPruning(ctx context.Context, runner *agent.Runner, modelID, text string, own int) (int, error) {
	for dropped := 0; ; {
		err := runner.Send(ctx, text)
		text = "" // history holds the prompt from here on
		switch {
		case runner.MaxOutputTokens > 0 && maxTokensRefused(err):
			capRejected.Store(modelID, true)
			runner.MaxOutputTokens = 0
		case contextTooLarge(err) || err == nil && runner.CutShort():
			if dropped > 0 || len(runner.History) < 3 {
				return own, err // nothing left to drop: the report is what it has
			}
			// keep the task (0) and the messages a tail of pairs from the end:
			// a call never loses its result
			start := 1
			for keep, at := 0, len(runner.History)-1; at > start && keep < 8; at-- {
				if runner.History[at].Role == fantasy.MessageRoleUser {
					keep = 0
					start = at
				} else {
					keep++
				}
			}
			h.Log.Warn("subagent context full, dropping history", "origin", "agent", "messages", start-1)
			dropped++
			runner.History = append(runner.History[:1:1], runner.History[start:]...)
			if own > 0 {
				own = max(1, own-(start-1))
			}
		default:
			return own, err
		}
	}
}

func (h *Host) subagentSystem(def AgentDef, toolset []fantasy.AgentTool, readOnly bool) string {
	read, readOnlyLine, update := "", "", ""
	if hasTool(toolset, tools.ToolRead) {
		read = readPrompt
	}
	if readOnly {
		readOnlyLine = "\n\nYou are read-only: you change no project file, only files in the scratchpad, and every command is denied. Report a change instead of making it."
	}
	if hasTool(toolset, tools.ToolUpdate) {
		update = "\n\nUse the update tool only when your caller's attention is needed now; it reads the line while you run and may steer you on it."
	}
	return "You are " + def.Name + ", a subagent of zemi. The caller sees only your final message, not your work:" +
		" end with a complete, concise report of what it needs, citing path:line where it matters," +
		" and say plainly what you did not find or could not verify.\n\n" + def.System + readOnlyLine + update +
		"\n\n# Using tools\n" + batchPrompt + read + "\n" + toolRules
}

// schemaReport cuts the outermost {...} out of a reply, which models wrap
// in prose and fences, and checks it against schema. What comes back is the
// checked object re-marshalled: the report the caller journals and answers
// with is what the gate validated, quotes and key order included.
func schemaReport(schema map[string]any, reply string) (string, error) {
	i, j := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if i < 0 || j < i {
		return "", errors.New("it holds no JSON object")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(reply[i:j+1]), &m); err != nil {
		return "", err
	}
	var req []string
	list, _ := schema["required"].([]any)
	for _, r := range list {
		name, _ := r.(string)
		req = append(req, name)
	}
	info := fantasy.ToolInfo{Parameters: tools.SchemaProperties(schema), Required: req}
	if err := tools.Validate(info, m, tools.SchemaOpen(schema)); err != nil {
		return "", err
	}
	// re-decode the raw bytes: values keep the model's own spelling, so no
	// number round-trips through float64 on its way to the journal
	var raw map[string]jsontext.Value
	if err := json.Unmarshal([]byte(reply[i:j+1]), &raw); err != nil {
		return "", err
	}
	out, err := json.Marshal(raw, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func lastAssistantText(history []fantasy.Message) string {
	for _, h := range slices.Backward(history) {
		if h.Role == fantasy.MessageRoleAssistant {
			if s := strings.TrimSpace(concatText(h.Content)); s != "" {
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
	h          *Host
	def        AgentDef
	run        *agentRun
	modelID    string
	progress   func(string)
	sid        string
	ctx        context.Context // the run's: cancelled, a call cut off was stopped
	stepText   int             // len(run.text) when the step began, where a retry restarts
	stepReason int             // len(run.reason) then, for the same retry
	base       int             // run.calls before this send: a continuation's row counts its own
}

func (s *subagentHooks) report(line string) {
	if s.progress != nil {
		s.progress(line)
	}
}

func (s *subagentHooks) TextDelta(text string) {
	s.run.mu.Lock()
	s.run.endThinking()
	s.run.text += text
	s.run.retryAt = time.Time{}
	s.run.mu.Unlock()
	s.h.refreshViewed(s.run)
}

// ReasoningDelta buffers a stretch of thinking: it settles as the one Thought
// row above the call or the reply that ends it, so the chat streams the tail
// of it the way the session's does, and a thought that ended never goes on
// spinning over the calls that follow it.
func (s *subagentHooks) ReasoningDelta(text string) {
	run := s.run
	run.mu.Lock()
	if run.reason == "" {
		// a late chunk joins the Thought row above it, as the session's does: a
		// provider sends a thought's last token after the call that ends it
		// started. A chunk over anything finished is a thought of its own, with
		// what streamed before it settled first.
		if !run.joinThought(text) {
			run.flush()
			run.beginThinking()
			run.reason += text
		}
	} else {
		run.reason += text
	}
	run.retryAt = time.Time{}
	run.mu.Unlock()
	s.h.refreshViewed(run)
}

// joinThought appends a late reasoning chunk to the Thought row above it when
// only calls of the step that have not finished lie between (run.mu held).
func (run *agentRun) joinThought(text string) bool {
	for i := len(run.tr) - 1; i >= 0; i-- {
		m := &run.tr[i]
		if m.Kind == RowReasoning {
			m.Text += text
			run.touch()
			return true
		}
		if m.Kind != RowTool || (m.Status != CallRun && m.Status != CallWait) {
			return false
		}
	}
	return false
}

// a run's row carries the command, which only the finished input holds
func (s *subagentHooks) ToolCallPending(string, string) {}
func (s *subagentHooks) ToolInputDelta(string, string)  {}

func (s *subagentHooks) ToolCallStart(id, name, input string) {
	diff := inputDiff(name, input)
	run := s.run
	run.mu.Lock()
	run.endThinking()
	run.flush()
	run.calls++
	run.retryAt = time.Time{}
	n := run.calls - s.base
	run.rows[id] = len(run.tr)
	run.tr = append(run.tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: tools.ToolName(name), Text: toolSummary(name, input), Label: toolLabel(input), Status: CallWait, Diff: diff})
	run.touch()
	run.mu.Unlock()
	// one line: the widget wraps, and a long command would push the chat around
	s.report(fmt.Sprintf("%s · %d tool %s · %s %s", s.def.Name, n, Plural(n, "call", "calls"), name,
		clipLine(toolSummary(name, input), 60)))
	s.h.refreshViewed(run)
}

func (s *subagentHooks) ToolCallRunning(id string) {
	run := s.run
	run.mu.Lock()
	if i, found := run.rows[id]; found {
		run.tr[i].Status = CallRun
		run.touch()
	}
	run.mu.Unlock()
	s.h.refreshViewed(run)
}

func (s *subagentHooks) ToolCallEnd(id, name string, result fantasy.ToolResultOutputContent, diff string) {
	run := s.run
	out, ok := agent.ToolOutput(result), !agent.ErrResult(result)
	// takeToolOutcome takes ag.mu, which nextTurn holds across this run's
	// run.mu: under run.mu here the pair deadlocks
	o := s.h.takeToolOutcome(id)
	run.mu.Lock()
	if i, found := run.rows[id]; found {
		t := &run.tr[i]
		t.Status, t.Output, t.Job = toolStatus(result, s.ctx != nil && s.ctx.Err() != nil, o), shortOutput(out), o.Job
		if o.Run != "" {
			t.Run = o.Run
		}
		switch {
		case diff != "":
			t.Diff = diff // shell: what the command changed
		case !ok:
			t.Diff = "" // the change did not happen
		}
		diff = t.Diff
		run.touch()
	}
	row := StreamTool{ID: id, Name: name, Diff: diff}
	if i, found := run.rows[id]; found {
		row.Status, row.Run, row.Job = run.tr[i].Status, run.tr[i].Run, run.tr[i].Job
	}
	run.mu.Unlock()
	s.h.setCallRow(id, rowMeta{Status: row.Status, Run: row.Run, Job: row.Job})
	s.h.refreshViewed(run)
	s.h.recordToolCall(s.sid, row, ok)
}

func (s *subagentHooks) StepStart() {
	s.run.mu.Lock()
	s.stepText, s.stepReason = len(s.run.text), len(s.run.reason)
	s.run.mu.Unlock()
}

func (s *subagentHooks) Retrying(err error, delay time.Duration) {
	run := s.run
	run.mu.Lock()
	run.text = run.text[:min(s.stepText, len(run.text))]
	run.reason = run.reason[:min(s.stepReason, len(run.reason))]
	if run.reason == "" {
		// the failed attempt's open thought goes with its text
		run.thinkAt = time.Time{}
	}
	run.retries++
	run.retryAt = time.Now().Add(delay)
	run.retryWhy, _ = s.h.explain(err)
	run.mu.Unlock()
	s.report(fmt.Sprintf("%s · retrying in %s", s.def.Name, fmtDuration(delay)))
	s.h.Log.Warn("subagent retry", "origin", "agents", "agent", s.def.Name, "run", run.id,
		"error", errString(err), "delay_ms", delay.Milliseconds())
	s.h.refreshViewed(run)
}

// StreamDone counts a model call on the run's row as its stream finishes:
// a subagent's whole run bills once at TurnDone.
func (s *subagentHooks) StreamDone(u fantasy.Usage) {
	s.run.mu.Lock()
	s.run.tokens += billedInput(u) + u.OutputTokens
	s.run.cost += s.h.turnCost(s.modelID, u)
	s.run.mu.Unlock()
}

func (s *subagentHooks) StepDone(st agent.Step) { s.h.emitStep(s.sid, s.modelID, st) }

func (s *subagentHooks) TurnDone(err error, usage fantasy.Usage) {
	h := s.h
	h.bill(s.run.parent, s.modelID, usage)
	if s.sid != "" {
		h.bill(s.sid, s.modelID, usage)
	}
	_, n, _, _ := s.run.state()
	n -= s.base
	s.report(fmt.Sprintf("%s · %d tool %s", s.def.Name, n, Plural(n, "call", "calls")))
	h.refresh()
}

// filterTools keeps the tools named in want (nil keeps all) minus skip, and
// reports the wanted names nothing answers to: a caller carrying another
// harness's spelling ("Read, Grep") would otherwise run with no tools at all
// and invent its report.
func filterTools(all []fantasy.AgentTool, want []string, skip set.Set[tools.ToolName]) (kept []fantasy.AgentTool, unknown []string) {
	var keep set.Set[string]
	if want != nil {
		keep = make(set.Set[string], len(want))
		for _, n := range want {
			keep.Add(n)
		}
	}
	return filterNamed(all, keep, nil, skip, want)
}

// filterRefs filters by a definition's tools list: a deny entry removes its
// tool, so a list that only removes still gets the tools a later release
// adds.
func filterRefs(all []fantasy.AgentTool, want []toolRef, skip set.Set[tools.ToolName]) (kept []fantasy.AgentTool, unknown []string) {
	keep, drop := make(set.Set[string], len(want)), make(set.Set[string], len(want))
	names := make([]string, len(want))
	for i, r := range want {
		names[i] = r.name
		if r.deny {
			drop.Add(r.name)
		} else {
			keep.Add(r.name)
		}
	}
	return filterNamed(all, keep, drop, skip, names)
}

// filterNamed keeps the tools named in keep (empty keeps all) minus drop and
// skip; a name in names nothing answers to lands in unknown.
func filterNamed(all []fantasy.AgentTool, keep, drop set.Set[string], skip set.Set[tools.ToolName], names []string) (kept []fantasy.AgentTool, unknown []string) {
	have := set.Set[string]{}
	for _, t := range all {
		name := t.Info().Name
		have.Add(name)
		if skip.Has(tools.ToolName(name)) || drop.Has(name) || (len(keep) > 0 && !keep.Has(name)) {
			continue
		}
		kept = append(kept, t)
	}
	for _, n := range names {
		if !have.Has(n) && !skip.Has(tools.ToolName(n)) {
			unknown = append(unknown, n)
		}
	}
	return kept, unknown
}

// childTools is a subagent run's tool set: the full set minus what a subagent
// never gets, filtered by the definition's allowlist, then the update tool,
// which lands after the filter so no allowlist takes the way back to its caller.
// Naming update there, or denying it, is accepted and ignored: every run holds it.
func childTools(h *Host, pipe *tools.Pipeline, want []toolRef) (kept []fantasy.AgentTool, unknown []string) {
	want = slices.DeleteFunc(want, func(r toolRef) bool { return r.name == string(tools.ToolUpdate) })
	kept, unknown = filterRefs(h.allTools(pipe), want, subagentExcluded)
	return append(kept, &updateTool{h: h, pipe: pipe}), unknown
}

func hasTool(toolset []fantasy.AgentTool, name tools.ToolName) bool {
	return slices.ContainsFunc(toolset, func(t fantasy.AgentTool) bool { return t.Info().Name == string(name) })
}

// setToolDetail updates the live progress line under a running tool row.
func (h *Host) setToolDetail(callID, detail string) {
	h.setTool(callID, func(t *StreamTool) { t.Detail = detail })
}

// setToolProgress updates a running shell call: its last line under the row,
// and the tail its view reads.
func (h *Host) setToolProgress(callID, line, tail string) {
	h.setTool(callID, func(t *StreamTool) { t.Detail, t.Tail = line, tail })
}

// setToolOutcome keeps what a call's tool reported beyond its text, read when
// the call ends.
func (h *Host) setToolOutcome(callID string, o tools.Outcome) {
	ag := h.agents
	ag.mu.Lock()
	if ag.outcomes == nil {
		ag.outcomes = map[string]tools.Outcome{}
	}
	ag.outcomes[callID] = o
	ag.mu.Unlock()
}

// setCallRow keeps a finished call's facts for the message that drew it.
func (h *Host) setCallRow(callID string, m rowMeta) {
	ag := h.agents
	ag.mu.Lock()
	if ag.rows == nil {
		ag.rows = map[string]rowMeta{}
	}
	ag.rows[callID] = m
	ag.mu.Unlock()
}

// takeCallRows are the facts of the calls a message draws, cleared as they
// are read.
func (h *Host) takeCallRows(msg fantasy.Message) map[string]rowMeta {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	var out map[string]rowMeta
	for _, p := range msg.Content {
		id := ""
		switch p := p.(type) {
		case fantasy.ToolCallPart:
			id = p.ToolCallID
		case fantasy.ToolResultPart:
			id = p.ToolCallID
		}
		if m, ok := ag.rows[id]; ok && id != "" {
			if out == nil {
				out = map[string]rowMeta{}
			}
			out[id] = m
			delete(ag.rows, id)
		}
	}
	return out
}

// takeToolOutcome is a finished call's outcome, cleared as it is read.
func (h *Host) takeToolOutcome(callID string) tools.Outcome {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	o := ag.outcomes[callID]
	delete(ag.outcomes, callID)
	return o
}

func (h *Host) setTool(callID string, set func(*StreamTool)) {
	ag := h.agents
	ag.mu.Lock()
	for i := range ag.tools {
		if ag.tools[i].ID == callID {
			set(&ag.tools[i])
		}
	}
	ag.mu.Unlock()
	h.agentStream()
}

// subagentModelID picks a subagent's model: the definition's reference (a
// target name or a provider/model id, which a workflow may set per run), then
// the agents role.
func (h *Host) subagentModelID(def AgentDef) string {
	return cmp.Or(h.modelRef(def.Model), h.roleModel(agentsRole))
}

// ModelIDs lists every provider/model the config registered.
func (h *Host) ModelIDs() []string {
	var out []string
	for _, spec := range h.Providers.List() {
		for _, m := range spec.Models {
			out = append(out, spec.ID+"/"+m.ID)
		}
	}
	return out
}

// checkModelID refuses a model reference config does not know, naming the
// ones it does. A target name always passes: an unset slot follows chat.
func (h *Host) checkModelID(id string) error {
	if id == "" || h.isTarget(id) || slices.Contains(h.ModelIDs(), id) {
		return nil
	}
	return fmt.Errorf("unknown model %q; targets: %s; configured models: %s", id, strings.Join(h.targetNames(), ", "), strings.Join(h.ModelIDs(), ", "))
}

// the agent tool

// agentTool is the model's entry point to subagents. Parallel: several
// calls in one step run concurrently.
type agentTool struct {
	tools.NoProviderOptions
	h    *Host
	pipe *tools.Pipeline
	defs []AgentDef
}

func (h *Host) agentTool(pipe *tools.Pipeline) fantasy.AgentTool {
	defs := h.agentDefs()
	return &agentTool{h: h, pipe: pipe, defs: defs}
}

// names lists the agents this tool offers.
func (t *agentTool) names() []string {
	out := make([]string, len(t.defs))
	for i, d := range t.defs {
		out[i] = d.Name
	}
	return out
}

// agentArgs is the agent tool's input and its schema source. Every field is
// optional in the schema: send_to makes prompt and agent spurious, and what
// still must be there the descriptions say.
type agentArgs struct {
	Description string `json:"description,omitempty" desc:"3-5 word label shown to the user"`
	Prompt      string `json:"prompt,omitempty" desc:"the full task"`
	Agent       string `json:"agent,omitempty" desc:"one of the Agents above"`
	Background  bool   `json:"background,omitempty" desc:"run detached; the report arrives later as a message"`
	SendTo      string `json:"send_to,omitempty" desc:"id of a run to continue, a workflow's agent too: a running one reads prompt at its next step, a finished one answers it with its history kept; agent and description are then ignored"`
	Wait        string `json:"wait,omitempty" desc:"a run id, or \"all\" for every agent run and workflow of this session: block until it, or the last of them, ends instead of working on; a wait returns early when a run sends an update - read it, answer with send_to if needed, then wait again"`
	Stop        bool   `json:"stop,omitempty" desc:"with send_to: kill the run; its report never arrives"`
}

func (t *agentTool) Info() fantasy.ToolInfo {
	var b strings.Builder
	b.WriteString("Start a subagent on a self-contained task. It cannot see this conversation, so the prompt carries everything it needs," +
		" what to report and how to verify. Only its final report comes back, and the user does not see it: relay what matters." +
		" A long run sends you lines with the update tool as it goes, which you read at your next step." +
		" Calls sent together run at once." +
		" Set background for long work you need not wait on. A call without it waits for its run, and hands the run off when the user sends a message while it runs or when the turn is interrupted: the report or result then arrives as a message." +
		" send_to continues a run, running or finished, by its run id; stop kills the run." +
		" wait blocks until a run, or every run and workflow of this session with \"all\", ends instead of working on; a run sending an update, or a sibling finishing or sending one, ends the wait early with a line naming it and the waited run, which keeps running: read it, answer with send_to if needed, then wait again.")
	b.WriteString("\n\nAgents:")
	for _, d := range t.defs {
		fmt.Fprintf(&b, "\n- %s: %s", d.Name, truncRunes(d.Description, 200))
		if d.Model != "" {
			fmt.Fprintf(&b, " (model %s)", d.Model)
		}
	}
	props, req := tools.ArgsOf(agentArgs{})
	for _, name := range []string{"description", "agent"} {
		p := props[name].(map[string]any)
		p["description"] = p["description"].(string) + "; required unless send_to or wait is set"
	}
	props["agent"].(map[string]any)["enum"] = t.names()
	return fantasy.ToolInfo{
		Name:        string(tools.ToolAgent),
		Description: b.String(),
		Parameters:  props,
		Required:    req,
		Parallel:    true,
	}
}

// bgAgentStarted opens the result of an agent call that went on in the
// background, whose row reads as still running (toolStatus).
const bgAgentStarted = "Started background agent "

// sessionRun looks a run of this session up by id, for send_to and wait: from
// the registry, else the store for one this process forgot. Another session's
// run is refused: its child session and bill belong to the conversation that
// started it.
func (h *Host) sessionRun(id string) (*agentRun, error) {
	run := h.tasks.find(id)
	if run == nil {
		var err error
		if run, err = h.loadRun(id); err != nil {
			return nil, err
		}
	}
	if run.parent != h.AgentSessionID() {
		return nil, fmt.Errorf("agent %s belongs to another session: start a fresh run", run.id)
	}
	return run, nil
}

func (t *agentTool) Run(ctx context.Context, call fantasy.ToolCall) (reply fantasy.ToolResponse, err error) {
	var in agentArgs
	if err := decodeInput(t.Info(), call.Input, &in); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if in.Wait != "" && in.Stop {
		return fantasy.NewTextErrorResponse("set one of wait or stop, not both"), nil
	}
	if in.Wait != "" && in.SendTo != "" {
		return fantasy.NewTextErrorResponse("set one of wait or send_to, not both"), nil
	}
	if in.Stop && in.SendTo == "" {
		return fantasy.NewTextErrorResponse("pass the run's id as send_to"), nil
	}
	if strings.TrimSpace(in.Prompt) != "" && in.Wait != "" {
		return fantasy.NewTextErrorResponse("wait takes no prompt: send one with send_to, or wait without one"), nil
	}
	if strings.TrimSpace(in.Prompt) == "" && !in.Stop && in.Wait == "" {
		return fantasy.NewTextErrorResponse("prompt is required"), nil
	}
	if resp, stop := t.h.preCheck(string(tools.ToolAgent), call.Input); stop {
		return resp, nil
	}
	defer t.h.postNotify(ctx, t.pipe.Pad, string(tools.ToolAgent), call.Input, &reply, time.Now())
	if in.SendTo != "" {
		run, err := t.h.sessionRun(in.SendTo)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		if in.Stop {
			if !run.isRunning() {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("no running agent %q", run.id)), nil
			}
			run.cancel()
			t.h.setToolDetail(call.ID, "stopped "+run.id)
			return fantasy.NewTextResponse("Stopped agent " + run.id + " (" + run.description + "); its report will not arrive."), nil
		}
		if run.isRunning() {
			if err := t.h.MessageAgent(run.id, in.Prompt); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			t.h.setToolDetail(call.ID, "message to "+run.id)
			return fantasy.NewTextResponse("Queued for agent " + run.id + ": it reads the message at its next step. Its report still arrives when it finishes."), nil
		}
		t.h.setTool(call.ID, func(st *StreamTool) { st.Run = run.id })
		return t.foreground(ctx, call.ID, run, func(progress func(string)) (string, error) {
			return t.h.continueRun(ctx, run, in.Prompt, progress)
		})
	}
	if in.Wait != "" {
		if in.Wait == "all" {
			return t.waitAll(ctx, call.ID), nil
		}
		run, err := t.h.sessionRun(in.Wait)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		return t.wait(ctx, call.ID, run), nil
	}
	if strings.TrimSpace(in.Agent) == "" {
		// only send_to may leave it out; an omission must not silently run
		// the agent with the most tools
		return fantasy.NewTextErrorResponse("agent is required: " + strings.Join(t.names(), ", ")), nil
	}
	if in.Agent == forkAgent {
		// a run of the conversation has nothing of its own to fork, and a
		// detached one would start after the turn it should continue
		if run := runOf(ctx); run != nil {
			return fantasy.NewTextErrorResponse(forkNested(run)), nil
		}
		if in.Background {
			return fantasy.NewTextErrorResponse("a fork starts from the turn that is running, so it cannot be detached: call it in the foreground, or set background for a fresh agent"), nil
		}
	}
	def, err := t.h.agentDef(in.Agent)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	in.Description = cmp.Or(strings.TrimSpace(in.Description), def.Name)
	if in.Background && t.h.hooks() != nil {
		id, err := t.h.startBackgroundAgent(ctx, def, in.Description, in.Prompt)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		t.h.setTool(call.ID, func(st *StreamTool) { st.Detail, st.Run = def.Name+" · background "+id+" (/agents)", id })
		t.h.setToolOutcome(call.ID, tools.Outcome{Run: id})
		return fantasy.NewTextResponse(bgAgentText(id)), nil
	}
	// the run outlives the call: a foreground one hands off to the user's
	// message, so its context cannot be the call's own
	run, rctx := t.h.newRun(context.WithoutCancel(ctx), def, in.Description, in.Prompt, false)
	_ = t.h.tasks.add(run)
	t.h.setTool(call.ID, func(st *StreamTool) { st.Run = run.id })
	return t.foreground(ctx, call.ID, run, func(progress func(string)) (string, error) {
		return t.h.runAgent(rctx, run, def, in.Prompt, progress)
	})
}

// wait blocks until a run of this session ends, the user types, or the call
// is stopped: the join a detached agent otherwise has none of. It answers
// with the run's status and never with its report, which the run's own end
// sends as a message. A sibling - another agent run or workflow of this
// session - that ends or sends an update wakes the wait too: the answer names
// the sibling and the run that still goes on, so the two are never confused.
func (t *agentTool) wait(ctx context.Context, callID string, run *agentRun) fantasy.ToolResponse {
	h := t.h
	answer := func() fantasy.ToolResponse {
		status, _, took, _ := run.state()
		where := fmt.Sprintf("agent %s %s (%s)", run.id, status, fmtDuration(took))
		if status == RunRunning {
			return fantasy.NewTextResponse(where + "; it goes on, and its report arrives as a message.")
		}
		return fantasy.NewTextResponse(where + "; its report arrives as a message of its own, and send_to continues it.")
	}
	if !run.isRunning() {
		return answer()
	}
	h.setTool(callID, func(st *StreamTool) { st.Run = run.id })
	sibs := h.watchSiblings(h.AgentSessionID(), run.id)
	defer sibs.stop()
	end := run.waitEnd()
	update, cancelUpdate := run.waitUpdate()
	defer cancelUpdate()
	for {
		// before any state read: a bump between the two is a wake this
		// iteration re-checks, never a missed one
		change := h.tasks.onChange()
		select {
		case <-end:
			return answer()
		case <-update:
			// an update ends the wait early: the run keeps going as it was, and
			// its report follows the path it already had
			if !run.isRunning() {
				return answer() // it ended as it updated: the run-end answer is the truth
			}
			h.setToolOutcome(callID, tools.Outcome{Run: run.id})
			return fantasy.NewTextResponse(updateWaitText(run))
		case <-h.userSpoke():
			// the user's own message takes the run: its report goes to the session
			if run.detach() {
				h.setToolOutcome(callID, tools.Outcome{Run: run.id})
				return fantasy.NewTextResponse(handoffText("agent", run.id, run.label(),
					"wait "+run.id+" again if you still need it"))
			}
			<-end
			return answer()
		case <-ctx.Done():
			// esc stops the wait, not the run
			h.setToolOutcome(callID, tools.Outcome{Run: run.id})
			return fantasy.NewTextResponse("Stopped waiting; agent " + run.id + " goes on. Call agent again with wait " + run.id + ", or read its report when it arrives.")
		case <-change:
			if !run.isRunning() {
				return answer() // it ended as a sibling moved: the run-end answer is the truth
			}
			if lines := h.sibMoves(sibs); len(lines) > 0 {
				h.setToolOutcome(callID, tools.Outcome{Run: run.id})
				return fantasy.NewTextResponse(sibText(lines,
					fmt.Sprintf("agent %s (%s)", run.id, run.label()),
					"wait "+run.id+" again"))
			}
			// a run this wait does not name moved (a new sibling, a workflow's
			// own agent): keep waiting
		}
	}
}

// updateWaitText answers a wait an update woke: the line reads below as a
// message, the run goes on, and the caller steers or waits again.
func updateWaitText(run *agentRun) string {
	return fmt.Sprintf("agent %s (%s) sent an update while you waited; it reads below as a message. The run goes on - steer it with send_to if it needs one, then wait again.", run.id, run.description)
}

// sibWatch is what a wait watches besides the run it waits on: which sibling
// runs and workflows of the session were going when the wait began, and each
// plain sibling run's update channel. A change wake diffs against the entry
// sets; a workflow's own agents are no sibling, since their reports go to the
// script, never to the conversation.
type sibWatch struct {
	parent string
	runs   map[string]bool
	flows  map[string]bool
	update map[string]<-chan struct{}
	cancel []func()
}

// watchSiblings starts watching parent's other plain runs and workflows for
// the wait of run id except ("" when the wait is a workflow's).
func (h *Host) watchSiblings(parent, except string) *sibWatch {
	w := &sibWatch{parent: parent, runs: map[string]bool{}, flows: map[string]bool{}, update: map[string]<-chan struct{}{}}
	for _, r := range h.tasks.runningOf(parent) {
		if r.id == except || r.workflow != "" {
			continue
		}
		w.runs[r.id] = true
		ch, cancel := r.waitUpdate()
		w.cancel = append(w.cancel, cancel)
		w.update[r.id] = ch
	}
	for _, f := range h.tasks.runningFlows(parent) {
		if f.id != except {
			w.flows[f.id] = true
		}
	}
	return w
}

func (w *sibWatch) stop() {
	for _, c := range w.cancel {
		c()
	}
}

// sibMoves names the watched siblings that moved since the wait began: an
// ended run or workflow, a run that sent an update - one line each, the id
// first, so a crowded session stays attributable.
func (h *Host) sibMoves(w *sibWatch) []string {
	var lines []string
	for _, r := range h.tasks.runsOf(w.parent) {
		if !w.runs[r.id] {
			continue
		}
		if status, _, _, _ := r.state(); status != RunRunning {
			lines = append(lines, fmt.Sprintf("agent %s (%s) %s", r.id, r.label(), sibWord(status)))
		}
	}
	for _, id := range slices.Sorted(maps.Keys(w.update)) {
		select {
		case <-w.update[id]:
		default:
			continue
		}
		if r := h.tasks.find(id); r != nil {
			if status, _, _, _ := r.state(); status == RunRunning {
				lines = append(lines, fmt.Sprintf("agent %s (%s) sent an update", id, r.label()))
			}
		}
	}
	for _, f := range h.tasks.flowList() {
		if f.parent != w.parent || !w.flows[f.id] {
			continue
		}
		if !f.isRunning() {
			lines = append(lines, fmt.Sprintf("workflow %s (%s) %s", f.id, f.description, sibWord(f.state())))
		}
	}
	return lines
}

// sibText answers a wait a sibling moved: the lines name what happened, still
// names the run the wait keeps waiting on, resume says how to wait again -
// so a sibling's end is never mistaken for the waited run's.
func sibText(lines []string, still, resume string) string {
	var b strings.Builder
	if len(lines) == 1 {
		fmt.Fprintf(&b, "%s while you waited on %s. It reads below as a message.", lines[0], still)
	} else {
		fmt.Fprintf(&b, "While you waited on %s, these moved:", still)
		for _, l := range lines {
			fmt.Fprintf(&b, "\n%s", l)
		}
		fmt.Fprintf(&b, "\nThey read below as messages, one each.")
	}
	fmt.Fprintf(&b, " %s still runs - handle what arrived if needed, then %s.", still, resume)
	return b.String()
}

// sibWord is the status word a sibling line reads: "done" says nothing to a
// model deciding what to do next.
func sibWord(s RunStatus) string {
	switch s {
	case RunDone:
		return "finished"
	case RunWarn:
		return "finished with a warning"
	case RunCancelled:
		return "was stopped"
	default:
		return string(s)
	}
}

// waitAll blocks until every agent run and workflow this session is running
// has ended, one of the agents sends an update, or the user types: the join a
// fleet of detached agents otherwise has none of. The reports arrive as
// messages of their own, so the answer only sums the runs.
func (t *agentTool) waitAll(ctx context.Context, callID string) fantasy.ToolResponse {
	h := t.h
	var waited []*agentRun // every run this wait covered, for the final sum
	var waitedF []*flowRun // every workflow it covered
	for {
		// before the snapshot: a bump between the two is a wake this
		// iteration re-snapshots, never a missed one
		change := h.tasks.onChange()
		// a workflow's own agents are the flow's business: the flow itself is
		// in the snapshot, and a handoff must not detach what reports to a
		// script, not to the conversation
		runs := slices.DeleteFunc(h.tasks.runningOf(h.AgentSessionID()), func(r *agentRun) bool { return r.workflow != "" })
		flows := h.tasks.runningFlows(h.AgentSessionID())
		if len(runs) == 0 && len(flows) == 0 {
			if len(waited) > 0 || len(waitedF) > 0 {
				var b strings.Builder
				b.WriteString("Every agent run and workflow you waited on has ended; each report arrives as a message of its own.")
				for _, r := range waited {
					status, _, took, _ := r.state()
					fmt.Fprintf(&b, "\n- agent %s %s (%s)", r.id, status, fmtDuration(took))
				}
				for _, f := range waitedF {
					fmt.Fprintf(&b, "\n- workflow %s %s", f.id, f.state())
				}
				return fantasy.NewTextResponse(b.String())
			}
			text := "No agents are running."
			var recent []string
			for _, r := range h.tasks.runsOf(h.AgentSessionID()) {
				if status, _, took, _ := r.state(); status != RunRunning {
					recent = append(recent, fmt.Sprintf("%s %s (%s)", r.id, status, fmtDuration(took)))
					if len(recent) == 3 {
						break
					}
				}
			}
			if len(recent) > 0 {
				text += " Recently: " + strings.Join(recent, ", ") + "."
			}
			return fantasy.NewTextResponse(text)
		}
		for _, r := range runs {
			if !slices.Contains(waited, r) {
				waited = append(waited, r)
			}
		}
		for _, f := range flows {
			if !slices.Contains(waitedF, f) {
				waitedF = append(waitedF, f)
			}
		}
		wake, resp, spoke := t.awaitRuns(ctx, callID, change, runs)
		if resp != nil {
			return *resp
		}
		if wake != nil {
			t.h.setToolOutcome(callID, tools.Outcome{Run: wake.id})
			return fantasy.NewTextResponse(updateWaitText(wake))
		}
		if spoke {
			// the user's own messages take the runs: their reports go to the
			// session. A run already ended as the user spoke detaches not, and
			// the wait re-snapshots; a flow detaches the same way, for its end
			// report reads as a message like an agent's
			type handedRun struct{ kind, id, label string }
			var handed []handedRun
			for _, r := range runs {
				if r.detach() {
					handed = append(handed, handedRun{"agent", r.id, r.label()})
				}
			}
			for _, f := range flows {
				if f.detach() {
					handed = append(handed, handedRun{"workflow", f.id, f.description})
				}
			}
			if len(handed) == 0 {
				continue // every run ended as the user spoke: re-snapshot
			}
			t.h.setToolOutcome(callID, tools.Outcome{Run: handed[0].id})
			if len(handed) == 1 {
				hr := handed[0]
				return fantasy.NewTextResponse(handoffText(hr.kind, hr.id, hr.label,
					"wait all again if you still need to hold"))
			}
			lines := make([]string, len(handed))
			for i, hr := range handed {
				lines[i] = fmt.Sprintf("%s %s (%s)", hr.kind, hr.id, hr.label)
			}
			return fantasy.NewTextResponse(fmt.Sprintf("The user's message took the turn while you waited; %s go on, and their reports arrive as messages. Handle the message, then wait all again if you still need to hold.", strings.Join(lines, ", ")))
		}
		// the registry changed and runs remain: wait again
	}
}

// awaitRuns waits once for a snapshot of running runs and reports what woke
// it: the run whose update fired, the response to answer with (nil to have
// the caller re-snapshot), or spoke when the user's own message arrived - the
// caller detaches, for the flows in its join are its to hand off too.
func (t *agentTool) awaitRuns(ctx context.Context, callID string, change <-chan struct{}, runs []*agentRun) (wake *agentRun, resp *fantasy.ToolResponse, spoke bool) {
	type waiter struct {
		run    *agentRun
		update chan struct{}
	}
	waiters := make([]waiter, len(runs))
	for i, r := range runs {
		ch, cancel := r.waitUpdate()
		defer cancel()
		waiters[i] = waiter{r, ch}
	}
	select {
	case <-change:
	case <-t.h.userSpoke():
		return nil, nil, true
	case <-ctx.Done():
		// esc stops the wait, not the runs
		if len(runs) > 0 {
			t.h.setToolOutcome(callID, tools.Outcome{Run: runs[0].id})
		}
		resp := fantasy.NewTextResponse("Stopped waiting; the agents go on.")
		return nil, &resp, false
	}
	// the bump's source is ambiguous: an update closes its waiter's channel,
	// so a change that was really an update is named here
	for _, w := range waiters {
		select {
		case <-w.update:
			return w.run, nil, false
		default:
		}
	}
	return nil, nil, false
}

// bgAgentText is what a call whose run reports as a message answers: a
// background start, or a foreground call the user's message handed off.
func bgAgentText(id string) string {
	return bgAgentStarted + id + ". Wait for it with wait=" + id + ", or read its report when it arrives as a message."
}

// handoffText answers a wait the user's own message took: the run was already
// going, so nothing started - the turn reads the message and the run reports
// to the conversation, where the wait can pick it up again.
func handoffText(kind, id, label, resume string) string {
	return fmt.Sprintf("The user's message took the turn while you waited on %s %s (%s); the run goes on, and its report arrives as a message. Handle the message, then %s.", kind, id, label, resume)
}

// foreground waits for a run and answers with its report, the row's
// detail following its progress; the run id heads the report (the tail
// may be the spill file's name) so the model can continue the run. The run
// outlives the call: the user's own message hands it off, and its report then
// arrives as one. Only the run's end and esc - the call's own context, watched
// here - keep the call attached, and a handoff that lost the race to the end
// reads the result inline. do runs the run on its own context, which is not the
// call's: it must survive the call.
func (t *agentTool) foreground(call context.Context, callID string, run *agentRun, do func(func(string)) (string, error)) (fantasy.ToolResponse, error) {
	start := time.Now()
	var last atomic.Value
	type answer struct {
		text string
		err  error
	}
	answers := make(chan answer, 1)
	t.h.GoSafe("agent run", func() {
		a := answer{}
		// deferred: a panic inside the run must not leave the call waiting for
		// an answer that never comes
		defer func() {
			if r := recover(); r != nil {
				a.err = fmt.Errorf("it panicked: %v", r)
				t.h.logPanic("agent run", r)
			}
			answers <- a
		}()
		a.text, a.err = do(func(line string) {
			last.Store(line)
			t.h.setToolDetail(callID, line)
		})
		if run.handedOff() {
			t.h.deliverAgent(run, a.text, a.err)
		}
	})
	var a answer
	update, cancelUpdate := run.waitUpdate()
	defer cancelUpdate()
	select {
	case a = <-answers:
	case <-call.Done():
		// an interrupt stops the turn, not the run: it hands off the way one a
		// message took does, and its report arrives as a message
		if run.detach() {
			return fantasy.NewTextResponse("The turn was interrupted while agent " + run.id + " (" + run.label() + ") ran; the run goes on in the background, and its report arrives as a message. Wait for it with wait=" + run.id + " or read the report when it lands."), nil
		}
		run.cancel()
		a = <-answers
	case <-t.h.userSpoke():
		if run.detach() {
			return fantasy.NewTextResponse(bgAgentText(run.id)), nil
		}
		a = <-answers
	case <-update:
		// an update ends the call early: the run is handed off the way one the
		// user's message took is, and its report arrives as a message
		if run.detach() {
			return fantasy.NewTextResponse(fmt.Sprintf("agent %s (%s) sent an update while it ran; it reads below. The run goes on - call agent with wait %q to wait for its end.", run.id, run.description, run.id)), nil
		}
		a = <-answers
	}
	summary, _ := last.Load().(string)
	if summary == "" {
		summary = run.agent
	}
	t.h.setToolDetail(callID, summary+" · "+fmtDuration(time.Since(start)))
	if a.err != nil {
		// the chat draws a failure's output, so the id rides its first line rather than a [run] head
		msg := "agent " + run.id + " failed: " + t.h.friendlyError(a.err)
		if errors.Is(a.err, context.Canceled) {
			// the user or the caller's own stop call cancelled it
			msg = "agent " + run.id + " was stopped; do not restart it unless asked"
		}
		if a.text != "" {
			msg += "\n\nIts last message before that:\n" + a.text
		}
		return fantasy.NewTextErrorResponse(msg), nil
	}
	return fantasy.NewTextResponse("[run " + run.id + "]\n\n" + t.h.truncOutput(t.pipe.Pad, "agent", a.text)), nil
}

// the update tool

// updateTool lets a subagent tell its caller something before the run ends.
// The line goes to the caller's turn like a report does, so a caller that is
// waiting on the run reads it at its next step and can steer on it.
type updateTool struct {
	tools.NoProviderOptions
	h    *Host
	pipe *tools.Pipeline
}

// updateArgs is the update tool's input and its schema source.
type updateArgs struct {
	Text string `json:"text" desc:"one or two lines: the finding, the blocker or the question"`
}

func (t *updateTool) Info() fantasy.ToolInfo {
	props, required := tools.ArgsOf(updateArgs{})
	return fantasy.ToolInfo{
		Name: string(tools.ToolUpdate),
		Description: "Tell your caller one or two lines when its attention is needed now: a question or a decision you cannot settle yourself, a blocker, or a finding it explicitly asked to hear at once." +
			" Never to narrate progress or announce completion - the final report is that." +
			" The caller may steer you in answer, so read what comes back and follow it. Do not repeat what you already sent.",
		Parameters: props,
		Required:   required,
		Parallel:   true,
	}
}

func (t *updateTool) Run(ctx context.Context, call fantasy.ToolCall) (reply fantasy.ToolResponse, err error) {
	var in updateArgs
	if err := decodeInput(t.Info(), call.Input, &in); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return fantasy.NewTextErrorResponse("text is required"), nil
	}
	if resp, stop := t.h.preCheck(string(tools.ToolUpdate), call.Input); stop {
		return resp, nil
	}
	run := runOf(ctx)
	if run == nil {
		return fantasy.NewTextErrorResponse("there is no caller to update"), nil
	}
	reply = fantasy.NewTextResponse("Sent to the caller.")
	defer t.h.postNotify(ctx, t.pipe.Pad, string(tools.ToolUpdate), call.Input, &reply, time.Now())
	// its own result is the only place a child learns the line never landed
	if err := t.h.deliverUpdate(run, text); err != nil {
		reply = fantasy.NewTextErrorResponse(err.Error())
	}
	return reply, nil
}

// decodeInput validates a host tool's input against its schema, so a wrong
// type is worded as it is for a builtin tool, then decodes the validated map
// (coercions applied) into in.
func decodeInput(info fantasy.ToolInfo, input string, in any) error {
	m, err := tools.ParseInput(info, input, false)
	if err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, in)
}

// preCheck runs the tool.pre events for a host-implemented tool: deny
// becomes an error result, halt stops the turn. A rewrite is refused: these
// tools run and gate their own input, so running the original would ignore it.
func (h *Host) preCheck(tool, input string) (fantasy.ToolResponse, bool) {
	var m map[string]any
	_ = json.Unmarshal([]byte(input), &m)
	decision, reason, _ := h.toolPreHook()(tool, m)
	if decision == tools.PreRewrite {
		return fantasy.NewTextErrorResponse("refused: a tool.pre rewrite is not supported for the " + tool + " tool"), true
	}
	return tools.VerdictResponse(decision, reason)
}

// postNotify is preCheck's other half: these tools run outside
// internal/tools, which runs tool.post for every other one. Deferred on
// the tool's named result, so every return path reports and a handler's
// rewrite reaches the model.
func (h *Host) postNotify(ctx context.Context, pad, tool, input string, reply *fantasy.ToolResponse, start time.Time) {
	var m map[string]any
	_ = json.Unmarshal([]byte(input), &m)
	if out := h.toolPost(ctx, pad, tool, m, !reply.IsError, reply.Content, nil, time.Since(start)); reply.Type == "text" {
		reply.Content = out
	}
}

// fmtDuration reads as the chat's durations do (tui text.duration).
func fmtDuration(d time.Duration) string {
	if d < 9950*time.Millisecond {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	switch s := int(d.Round(time.Second).Seconds()); {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %02ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh %02dm", s/3600, s/60%60)
	}
}

// /agents and zemi.agent.run

// newAgentPrompt rides behind /agents new and takes zemiSkillDir, where the
// procedure lives.
const newAgentPrompt = "I want a new zemi subagent that does what the command above says; ask me when it says nothing. Read SKILL.md in %s, the zemi skill's directory, then follow the New agent steps in agents.md there."

// agentsCommands is /agents.
func (h *Host) agentsCommands() []*Command {
	return []*Command{{
		Name: "agents", NArgs: NArgsAny, Desc: "see running agents and the ones zemi can start, or make one",
		GoFn: func(h *Host, args string, confirm bool) error {
			typed, attached := agent.CutAttached(args)
			sub, id, _ := strings.Cut(strings.TrimSpace(typed), " ")
			switch id = strings.TrimSpace(id); sub {
			case "":
				return h.openAgents()
			case "new":
				// the line as typed, newlines included, with the prompt behind it
				return h.sendCommandPrompt("agents", strings.TrimSpace(typed), agent.Attached("command", fmt.Sprintf(newAgentPrompt, h.zemiSkillDir())), attached)
			case "open":
				return h.openAgentFile(id)
			case "show":
				return h.showAgent(id)
			case "view":
				// a screen's v names the screen it came from, which esc
				// reopens over the session: show the run's preview, list the
				// agents list
				id, from, _ := strings.Cut(id, " ")
				switch from {
				case "":
					return h.ViewAgent(id)
				case "show", "list":
					back := "agents"
					if from == "show" {
						back = "agents show " + id
					}
					return h.ViewAgentFrom(id, back, "")
				}
				return Usagef("/agents view <id> [show|list]")
			case "stop":
				return h.stopRun(id)
			}
			return Usagef("unknown /agents subcommand %q (new [what it does], open <name>, show <id>, view <id> [show|list], stop [id])", sub)
		},
		GoComplete: SubcommandCompleter([]string{"new", "open", "show", "view", "stop"}, func(h *Host, sub string) []string {
			if sub != "open" {
				return nil
			}
			var names []string
			for _, d := range h.agentDefs() {
				if d.Path != "" {
					names = append(names, d.Name)
				}
			}
			return names
		}),
	}}
}

// openAgentFile opens an agent's file in the user's editor.
func (h *Host) openAgentFile(name string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	if name == "" { // agentDef reads "" as the default agent
		return Usagef("/agents open <name>")
	}
	d, err := h.agentDef(name)
	switch {
	case err != nil:
		return Usagef("%v", err)
	case d.Name == forkAgent:
		return Usagef("fork is not a file: it continues the conversation it is asked from, so there is nothing to edit")
	case d.Path == "":
		return Usagef("%s is builtin: agents/%s.md in %s replaces it", name, name, perm.TildePath(h.Dirs.ConfigHome))
	}
	h.openInEditor(ui, d.Path, "the agent")
	return nil
}

func (h *Host) openAgents() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	ui.OpenOverlay(h.agentsOverlay())
	h.watchAgents()
	return nil
}

// showAgent opens a run's preview: the view a /agents row's enter opens,
// from a click on the chat's agent row. v still opens the transcript.
func (h *Host) showAgent(id string) error {
	if id == "" {
		return Usagef("/agents show <id>")
	}
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	run := h.tasks.find(id)
	if run == nil {
		if run, err = h.loadRun(id); err != nil {
			return err
		}
	}
	ui.OpenOverlay(h.agentShowOverlay(run))
	h.watchAgents()
	return nil
}

// agentShowOverlay is a run's preview: the view a /agents row's enter opens,
// and the one a chat click opens - a rail action's own view, so the walk
// lands on it and ctrl+j/k step along the rail around its row. The watch
// rebuilds it while the run lives; v shows the transcript.
func (h *Host) agentShowOverlay(run *agentRun) Overlay {
	status, calls, took, _ := run.state()
	tokens, cost := run.usage()
	fields, detail, parts, pending := h.agentView(agentRow{id: run.id, def: run.agent, status: status, calls: calls, tokens: tokens, cost: cost, took: took}, run)
	o := Overlay{
		ID: "agent:" + run.id, Title: run.label(), Origin: lua.OriginBuiltin, Wide: true,
		Mark: RunMarks[string(status)], Fields: fields, Body: detail, Parts: parts, Empty: pending,
		Keys: []OverlayKey{{Key: "v", Desc: "view its transcript", Run: "agents view " + run.id + " show"}},
	}
	o.Prev, o.Next = h.railStep(run.id)
	return o
}

// agentsOverlay is /agents: the runs this process holds, then one row per
// agent file, so a run's elapsed time and state are read where they are.
func (h *Host) agentsOverlay() Overlay {
	defs := h.agentDefs()
	runs := h.agentRunItems()
	sel := 0
	if len(runs) > 0 && runs[0].Section == sectionRunning { // runs list running first
		sel = 1
	}
	items := append([]OverlayItem{{Text: "New agent", Verb: VerbFill, Fill: "/agents new "}}, runs...)
	for _, d := range defs {
		model, tools, access := cmp.Or(d.Model, "agents"), "every tool a subagent may use", "can change files"
		if d.Tools != nil {
			named := make([]string, len(d.Tools))
			for i, r := range d.Tools {
				if r.deny {
					named[i] = "-" + r.name
				} else {
					named[i] = r.name
				}
			}
			tools = strings.Join(named, ", ")
		}
		if d.ReadOnly {
			access = "read only"
		}
		fields := []string{"source", h.sourceOf(d.Source, d.Path), "model", model, "tools", tools, "access", access}
		if d.Name == forkAgent {
			// nothing of its own to describe: it sends the caller's own
			fields = []string{"source", h.sourceOf(d.Source, d.Path), "prompt, tools, model", "the caller's"}
		}
		item := OverlayItem{
			Text:    d.Name,
			Desc:    listDesc(d.Description),
			Section: sourceSection(d.Source) + " agents", // apart from the runs above
			Detail:  d.Description,
		}
		if d.Path != "" {
			fields = append(fields, "path", perm.TildePath(d.Path))
			item.Keys = []ItemKey{{Key: "o", Desc: openInEditorLabel, Run: "agents open " + d.Name}}
		}
		item.Fields = fieldList(fields...)
		items = append(items, item)
	}
	return Overlay{ID: "agents", Title: "Agents", Origin: lua.OriginBuiltin, Items: items, Selected: sel, Verb: "open", Wide: true}
}

// watchAgents keeps /agents and the open run previews current while a run
// lives: their elapsed time, state and marks come from the runs, which the
// rows snapshot when they are built.
func (h *Host) watchAgents() {
	h.overlayTicker("agents view", h.claimAgentsView, func() bool {
		running := h.tasks.running()
		if !running {
			h.agentsView.Store(false)
		}
		h.refreshAgents()
		return running
	})
}

// refreshAgents rebuilds /agents and the run previews that are open.
func (h *Host) refreshAgents() {
	ui := h.hooks()
	if ui == nil {
		return
	}
	ui.RefreshOverlay("agents", h.agentsOverlay)
	for _, run := range h.tasks.runList() {
		ui.RefreshOverlay("agent:"+run.id, func() Overlay { return h.agentShowOverlay(run) })
	}
}

// claimAgentsView takes the agents view's one ticker: false while it runs.
func (h *Host) claimAgentsView() bool { return h.agentsView.CompareAndSwap(false, true) }

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
		onDone, _ := arg[*glua.LFunction](L, 2, "zemi.agent.run")
		prompt := rawString(L, spec, "prompt")
		if strings.TrimSpace(prompt) == "" {
			L.RaiseError("zemi.agent.run: prompt is required")
		}
		h.luaGate(L, luaAgentRun, clipRunes(prompt, 40), func() map[string]any {
			return map[string]any{"prompt": prompt, "agent": rawString(L, spec, "agent")}
		})
		def, err := h.agentDef(rawString(L, spec, "agent"))
		if err != nil {
			L.RaiseError("zemi.agent.run: %v", err)
		}
		desc := rawString(L, spec, "description")
		if desc == "" {
			desc = clipRunes(prompt, 40)
		}
		// the run outlives the call that started it, but not its origin: a run
		// told to fork must see the run it was asked from
		ctx, cancel := context.WithCancel(context.WithoutCancel(cmp.Or(L.Context(), context.Background())))
		at := h.callerOf(L)
		var text string
		var runErr error
		h.asyncTask(L, asyncTaskSpec{
			subject: "zemi.agent.run", waitAPI: "run:wait", guard: "agent:",
			ctx: ctx, cancel: cancel, withCancel: true, res: &runErr,
			body: func(ctx context.Context) {
				_, t, err := h.runSubagent(ctx, def, desc, prompt)
				text, runErr = t, nil
				if err != nil {
					runErr = errors.New(h.friendlyError(err))
				}
				switch {
				case onDone == nil:
				case err != nil:
					h.callLuaAsync("zemi.agent.run on_done", at, onDone, nil, runErr.Error())
				default:
					h.callLuaAsync("zemi.agent.run on_done", at, onDone, text, nil)
				}
			},
			ret: func(L *glua.LState, err error) []glua.LValue {
				if err != nil {
					return []glua.LValue{glua.LNil, glua.LString(err.Error())}
				}
				if runErr != nil {
					return []glua.LValue{glua.LNil, glua.LString(runErr.Error())}
				}
				return []glua.LValue{glua.LString(text)}
			},
		})
		return 1
	}))
}
