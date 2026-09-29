// subagent.go runs subagents (DESIGN §6.3, M5): agent definitions, the
// model's agent tool, and the headless runs that zemi.agent.run and
// workflows share. A subagent is a fresh runner with its own history and a
// restricted tool set; its tool calls pass the same permission pipeline as
// the main agent's, and only its final message returns to the caller.

package hostapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/metafates/zemi/internal/skill"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// AgentDef is one subagent shape: builtin, or a markdown file under
// <config>/agents, a plugin's agents or <repo>/.agents/agents (frontmatter +
// system prompt).
type AgentDef struct {
	Name        string
	Description string
	// Model is a role name or a provider/model id; "" takes models.agents.
	Model    string
	Tools    []string // nil = every tool a subagent may use
	System   string
	Source   lua.Origin
	Path     string // the file, "" for a builtin
	ReadOnly bool   // file writes denied even when a tool could make one
}

// subagentExcluded never reaches a subagent: nesting, the parent session's
// todo list, the user's attention (questions, plan approval) and the main
// agent's lua tool. Listing one in an agent file drops it.
var subagentExcluded = map[string]bool{tools.ToolAgent: true, tools.ToolWorkflow: true, tools.ToolTodos: true, tools.ToolQuestion: true, tools.ToolPlan: true, tools.ToolLua: true}

var builtinAgents = []AgentDef{
	{
		Name:        "explore",
		Description: "Read-only: finds files, symbols and how something works. Cannot edit or run commands.",
		Tools:       []string{"read", "glob", "grep", "lsp", "fetch", "web_search", "skill"},
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
		Name:        "web",
		Description: "Searches the web, reads pages and follows links, answering with its sources. Give it the question, not just a URL.",
		Tools:       []string{"web_search", "fetch", "read", "grep", "glob"},
		ReadOnly:    true,
		System:      "Break the question into short, specific searches, fetch the pages that look relevant and follow their links when they hold the answer. A long page is saved to a file the fetch result names: grep and read that file instead of fetching again. Answer only what was asked, quote the exact text when it matters, and end with a Sources list of the URLs that contributed.",
		Source:      lua.OriginBuiltin,
	},
	{
		// the planner a read-only agent delegates to in a planning session (§6): a
		// read-only architect persona whose report is a concrete plan the
		// sending agent turns into a plan tool call.
		Name:        "plan",
		Description: "Read-only: explores the code and drafts an implementation plan with a Critical Files section, for a large or unclear change.",
		Tools:       []string{"read", "glob", "grep", "lsp", "skill", "fetch", "web_search"},
		ReadOnly:    true,
		System:      "Read the code the change touches and the patterns it should reuse; never guess what a read would answer. Then report a plan someone could implement without guessing: the goal, what changes where (files, and code to reuse as path:line), how to verify, the assumptions, and only the approach you recommend. End with a Critical Files section naming the 3-5 files that matter most, then the open questions only the user can answer.",
		Source:      lua.OriginBuiltin,
	},
}

const maxAgentFileBytes = 1 << 20

// agentDefs lists the available definitions. On a name clash the higher
// trust source wins: user over builtin over plugin over project. Project
// agents load only once the project config tree is trusted.
func (h *Host) agentDefs() []AgentDef {
	defs := slices.Clone(builtinAgents)
	h.layerFiles("agents", ".md", func(f layerFile) bool {
		d, err := parseAgentFile(f.name, f.path, f.origin)
		if err != nil {
			h.RecordErrorOnce("agents", err.Error())
			return false
		}
		defs = append(defs, d)
		return true
	})
	sortBySource(defs, func(d AgentDef) lua.Origin { return d.Source })
	seen := map[string]bool{}
	return slices.DeleteFunc(defs, func(d AgentDef) bool {
		dup := seen[d.Name]
		seen[d.Name] = true
		return dup
	})
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
			return AgentDef{}, fmt.Errorf("agent %q is in %s, and this project is not trusted: the user runs %s to load it", name, dir, h.trustHint())
		}
	}
	return AgentDef{}, fmt.Errorf("unknown agent %q (have %s)", name, strings.Join(names, ", "))
}

func parseAgentFile(name, path string, source lua.Origin) (AgentDef, error) {
	block, body, err := readFrontmatterFile(path)
	if err != nil {
		return AgentDef{}, err
	}
	var fm struct {
		Description string `yaml:"description"`
		Model       string `yaml:"model"`
		Tools       any    `yaml:"tools"`
		ReadOnly    bool   `yaml:"read_only"`
	}
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return AgentDef{}, fmt.Errorf("%s: frontmatter: %w", path, err)
	}
	d := AgentDef{
		Name:        name,
		Description: strings.TrimSpace(fm.Description),
		Model:       fm.Model,
		System:      strings.TrimSpace(body),
		Source:      source,
		Path:        path,
		ReadOnly:    fm.ReadOnly,
	}
	if d.Model == "inherit" {
		d.Model = ""
	}
	if d.Description == "" {
		return AgentDef{}, fmt.Errorf("%s: description is required", path)
	}
	// tools: "read, grep" or a yaml list
	switch t := fm.Tools.(type) {
	case string:
		for n := range strings.SplitSeq(t, ",") {
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
	block, body, err = skill.SplitFrontmatter(data)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", path, err)
	}
	return block, body, nil
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

// runAgent runs a registered run, recorded as a child session of the
// session that started it.
func (h *Host) runAgent(ctx context.Context, run *agentRun, def AgentDef, prompt string, progress func(string)) (text string, err error) {
	defer func() {
		run.finish(h, err)
		run.cancel()
		h.refresh()
	}()
	h.refresh()
	prof := &Profile{Model: h.subagentModelID(def)}
	model, err := h.resolveModel(prof)
	if err != nil {
		return "", err
	}
	modelID := h.currentModelID(prof)
	pipe := h.PipelineFor()
	pipe.TodosRead, pipe.TodosWrite, pipe.Question, pipe.ApprovePlan = nil, nil, nil, nil
	pipe.Images = func() bool { return h.takesImages(modelID) }
	pipe.Seen = &sync.Map{} // what the main agent read says nothing about what this run saw
	var sent sync.Map       // likewise the instruction files its reads carried
	pipe.Instructions = func(path string) string {
		return h.nearestInstructions(path, h.instructionFiles(), func(p string) bool {
			_, dup := sent.LoadOrStore(p, true)
			return dup
		})
	}
	// a run the planner starts is read-only too, whatever its definition says
	readOnly := def.ReadOnly || pipe.Plan()
	pipe.Plan = func() bool { return readOnly }
	// its session's pad even when the live session moved on (a background
	// run, a continuation after /new)
	if run.parent != "" {
		pipe.Pad = h.scratchFor(run.parent)
	}
	// A run outside any session (Lua at idle) still gets a parent, so
	// /sessions never lists it as a top-level session.
	parent := cmp.Or(run.parent, "lua")
	// memories name the session this run belongs to, not whichever is live
	// when a background run gets to them
	pipe.Remember = func(p *tools.Pipeline, scope store.Scope, text, quote, replaces string) (string, error) {
		return h.memoryTool(p, parent, scope, text, quote, replaces)
	}
	if ask := pipe.Ask; ask != nil {
		who := def.Name + " agent"
		if f := h.tasks.flow(run.workflow); f != nil {
			who = "workflow " + f.id + " · " + who // its descriptions are the model's words
		}
		pipe.Ask = func(req tools.Request) (bool, string, string) {
			req.Agent = who
			return ask(req)
		}
	}
	all := append(append(tools.All(pipe), h.luaFantasyTools(pipe)...), h.mcpFantasyTools(pipe)...)
	kept, unknown := filterTools(all, def.Tools, h.toolSkip(subagentExcluded))
	if len(unknown) > 0 {
		return "", fmt.Errorf("agent %s: no tool is named %s", def.Name, strings.Join(unknown, ", "))
	}
	loaded := new(sync.Map) // what the main agent loaded says nothing about what this run needs
	kept, active := h.deferMCP(kept, loaded)
	runner := agent.NewRunner(model, kept, h.systemWith(h.subagentSystem(def, kept, readOnly), kept, pipe.Pad, readOnly))
	runner.ActiveTools = active
	runner.MaxOutputTokens = h.maxOutput(modelID)
	window := h.contextWindow(modelID)
	runner.Full = func(used int64) bool { return window > 0 && used >= compactThreshold(window) }

	run.mu.Lock()
	prev, sid, base := run.runner, run.sessionID, run.calls
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
		// the run id heads the session id, so send_to finds the rows of a
		// run the process no longer holds (a resume, or one past maxKeptRuns)
		sid = run.id + store.NewID()[len(run.id):]
		if err := h.DB.CreateSession(sid, parent, run.description, def.Name); err != nil {
			h.Log.Warn("subagent session not recorded", "origin", "agents", "error", err)
			sid = ""
		}
		run.mu.Lock()
		run.sessionID = sid
		run.mu.Unlock()
	}
	// the run's own session keys its cache: its prompt is not the parent's
	runner.ProviderOptions, runner.Headers = h.Providers.CallOptions(modelID, h.Effort(modelID), sid), agent.SessionHeaders(sid)
	seedLoaded(loaded, runner.History)
	ctx = context.WithValue(ctx, runSessionKey{}, sid)
	hooks := &subagentHooks{h: h, def: def, run: run, modelID: modelID, progress: progress, sid: sid, ctx: ctx, base: base}
	runner.Hooks = hooks
	if sid != "" {
		runner.Persist = func(msg fantasy.Message) error {
			parts, err := json.Marshal(msg)
			if err != nil {
				return err
			}
			return h.DB.AddMessage(&store.Message{SessionID: sid, Role: string(msg.Role), PartsJSON: string(parts)})
		}
	}
	// in the task, not the system prompt, which runs of one agent share
	writes := hasTool(kept, tools.ToolShell) || hasTool(kept, tools.ToolWrite) || hasTool(kept, tools.ToolEdit)
	if len(runner.History) == 0 && pipe.Pad != "" && writes {
		prompt += "\n\n" + fmt.Sprintf(scratchOwn, filepath.Join(pipe.Pad, def.Name+"-"+run.id))
	}
	h.EmitAsync("agent.subagent.start", map[string]any{"agent": def.Name, "description": run.description, "session": sid})
	runner.Steer = run.takeQueue
	err = h.sendPruning(ctx, runner, modelID, prompt)
	for next := run.followUp(err); next != ""; next = run.followUp(err) {
		err = h.sendPruning(ctx, runner, modelID, next)
	}
	// NOTE: tools.Validate checks required fields and types, not every
	// keyword (enum, bounds); a deeper validator is the upgrade.
	var report string
	for tries := 0; err == nil && run.schema != nil; tries++ {
		var bad error
		if report, bad = schemaReport(run.schema, lastAssistantText(runner.History)); bad == nil {
			break
		}
		if tries == 2 {
			err = fmt.Errorf("the report is not the JSON asked for: %w", bad)
			break
		}
		err = h.sendPruning(ctx, runner, modelID, "Your reply is not the JSON asked for: "+bad.Error()+". Reply with only the JSON object.")
	}
	h.EmitAsync("agent.subagent.finish", map[string]any{"agent": def.Name, "session": sid, "error": errString(err)})
	// history keeps the steps finished before a failure: text is then the
	// last thing the agent said, for callers that show a partial report
	text = lastAssistantText(runner.History)
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

// sendPruning runs one subagent turn inside the context window: a run that
// fills it clears its old tool output and goes on, and a cap the model
// refuses is dropped once. A subagent has no summarizer: its history is its
// own and only its report leaves the run, so pruning is all it needs. With
// nothing to clear it drops the middle of the history, once: the task and a
// recent tail stay.
func (h *Host) sendPruning(ctx context.Context, runner *agent.Runner, modelID, text string) error {
	for dropped := false; ; {
		err := runner.Send(ctx, text)
		text = "" // history holds the prompt from here on
		switch {
		case runner.MaxOutputTokens > 0 && maxTokensRefused(err):
			capRejected.Store(modelID, true)
			runner.MaxOutputTokens = 0
		case contextTooLarge(err) || err == nil && runner.CutShort():
			window := h.contextWindow(modelID)
			history, freed := pruneToolOutput(runner.History, window, true)
			if freed == 0 {
				// tailStart never lands on a tool result, so the tail opens
				// with a message a provider accepts after the task.
				start := 1 + tailStart(history[1:], tailKeep(compactThreshold(window)))
				if dropped || start == 1 {
					return err // nothing left to drop: the report is what it has
				}
				// NOTE: the dropped steps are lost unsummarized, a follow-up
				// message older than the tail among them, and a run that
				// fills the window again ends with the error.
				h.Log.Warn("subagent context full, dropping history", "origin", "agent", "messages", start-1)
				dropped, history = true, append(history[:1:1], history[start:]...)
			}
			runner.History = history
		default:
			return err
		}
	}
}

func (h *Host) subagentSystem(def AgentDef, toolset []fantasy.AgentTool, readOnly bool) string {
	read, readOnlyLine := "", ""
	if hasTool(toolset, tools.ToolRead) {
		read = readPrompt
	}
	if readOnly {
		readOnlyLine = "\n\nYou are read-only: you change no project file, only files in the scratchpad, and commands run with the project unwritable. Report a change instead of making it."
	}
	return "You are " + def.Name + ", a subagent of zemi. The caller sees only your final message, not your work:" +
		" end with a complete, concise report of what it needs, citing path:line where it matters," +
		" and say plainly what you did not find or could not verify.\n\n" + def.System + readOnlyLine +
		"\n\n# Using tools\n" + batchPrompt + read + "\n" + toolRules
}

// schemaReport cuts the outermost {...} out of a reply, which models wrap
// in prose and fences, and checks it against schema.
func schemaReport(schema map[string]any, reply string) (string, error) {
	i, j := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if i < 0 || j < i {
		return "", errors.New("it holds no JSON object")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(reply[i:j+1]), &m); err != nil {
		return "", err
	}
	req := []string{}
	list, _ := schema["required"].([]any)
	for _, r := range list {
		name, _ := r.(string)
		req = append(req, name)
	}
	info := fantasy.ToolInfo{Parameters: tools.SchemaProperties(schema), Required: req}
	return reply[i : j+1], tools.Validate(info, m, tools.SchemaOpen(schema))
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
	h        *Host
	def      AgentDef
	run      *agentRun
	modelID  string
	progress func(string)
	sid      string
	ctx      context.Context // the run's: cancelled, a call cut off was stopped
	stepText int             // len(run.text) when the step began, where a retry restarts
	thinkAt  time.Time       // when the Thought row being streamed began, zero once it ended
	base     int             // run.calls before this send: a continuation's row counts its own
}

func (s *subagentHooks) report(line string) {
	if s.progress != nil {
		s.progress(line)
	}
}

func (s *subagentHooks) TextDelta(text string) {
	s.run.mu.Lock()
	s.endThinking()
	s.run.text += text
	s.run.mu.Unlock()
	s.h.refreshViewed(s.run)
}

// ReasoningDelta keeps one collapsed Thought row per stretch of thinking,
// under what streamed before it. A chunk after text with no call between
// joins the row above, as the main agent's does, so the reply stays whole.
func (s *subagentHooks) ReasoningDelta(text string) {
	run := s.run
	run.mu.Lock()
	if n := len(run.tr); n == 0 || run.tr[n-1].Kind != RowReasoning {
		run.flush()
	}
	if n := len(run.tr); n == 0 || run.tr[n-1].Kind != RowReasoning {
		run.tr = append(run.tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowReasoning})
		s.thinkAt = time.Now()
	}
	run.tr[len(run.tr)-1].Text += text
	run.mu.Unlock()
	s.h.refreshViewed(run)
}

// a run's row carries the command, which only the finished input holds
func (s *subagentHooks) ToolCallPending(string, string) {}
func (s *subagentHooks) ToolInputDelta(string, string)  {}

// endThinking times the Thought row the next text or call ends, as the
// main agent's is timed (run.mu held).
func (s *subagentHooks) endThinking() {
	if n := len(s.run.tr); !s.thinkAt.IsZero() && n > 0 && s.run.tr[n-1].Kind == RowReasoning {
		s.run.tr[n-1].Stat = &TurnStat{ThoughtMS: time.Since(s.thinkAt).Milliseconds()}
	}
	s.thinkAt = time.Time{}
}

func (s *subagentHooks) ToolCallStart(id, name, input string) {
	diff := inputDiff(name, input)
	run := s.run
	run.mu.Lock()
	s.endThinking()
	run.flush()
	run.calls++
	n := run.calls - s.base
	run.rows[id] = len(run.tr)
	run.tr = append(run.tr, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: name, Text: toolSummary(name, input), Label: toolLabel(input), Status: CallWait, Diff: diff})
	run.mu.Unlock()
	// one line: the widget wraps, and a long command would push the chat around
	s.report(fmt.Sprintf("%s · %d tool %s · %s %s", s.def.Name, n, plural(n, "call", "calls"), name,
		clipRunes(strings.Join(strings.Fields(toolSummary(name, input)), " "), 60)))
	s.h.refreshViewed(run)
}

func (s *subagentHooks) ToolCallRunning(id string) {
	run := s.run
	run.mu.Lock()
	if i, found := run.rows[id]; found {
		run.tr[i].Status = CallRun
	}
	run.mu.Unlock()
	s.h.refreshViewed(run)
}

func (s *subagentHooks) ToolCallEnd(id, name string, ok bool, out, diff string) {
	run := s.run
	run.mu.Lock()
	if i, found := run.rows[id]; found {
		t := &run.tr[i]
		t.Status, t.Output = toolStatus(name, ok, out, s.ctx != nil && s.ctx.Err() != nil), shortOutput(out)
		switch {
		case diff != "":
			t.Diff = diff // shell: what the command changed
		case !ok:
			t.Diff = "" // the change did not happen
		}
		diff = t.Diff
	}
	run.mu.Unlock()
	s.h.refreshViewed(run)
	// memoryAuto reads the child's tools: a web agent's report is outside text
	s.h.recordToolCall(s.sid, StreamTool{ID: id, Name: name, Diff: diff}, ok)
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
	s.report(fmt.Sprintf("%s · retrying in %s", s.def.Name, fmtDuration(delay)))
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
	s.report(fmt.Sprintf("%s · %d tool %s", s.def.Name, n, plural(n, "call", "calls")))
	h.refresh()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// filterTools keeps the tools named in want (nil keeps all) minus skip, and
// reports the wanted names nothing answers to: an agent file carrying
// another harness's spelling ("Read, Grep") would otherwise run with no
// tools at all and invent its report. A "-name" entry removes that tool
// instead, so a list that only removes still gets the tools a later
// release adds.
func filterTools(all []fantasy.AgentTool, want []string, skip map[string]bool) (kept []fantasy.AgentTool, unknown []string) {
	keep, drop, have := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, n := range want {
		if name, cut := strings.CutPrefix(n, "-"); cut {
			drop[name] = true
		} else {
			keep[n] = true
		}
	}
	for _, t := range all {
		name := t.Info().Name
		have[name] = true
		if skip[name] || drop[name] || (len(keep) > 0 && !keep[name]) {
			continue
		}
		kept = append(kept, t)
	}
	for _, n := range want {
		name := strings.TrimPrefix(n, "-")
		if !have[name] && !skip[name] {
			unknown = append(unknown, name)
		}
	}
	return kept, unknown
}

func hasTool(toolset []fantasy.AgentTool, name string) bool {
	return slices.ContainsFunc(toolset, func(t fantasy.AgentTool) bool { return t.Info().Name == name })
}

// setToolDetail updates the live progress line under a running tool row.
func (h *Host) setToolDetail(callID, detail string) {
	h.setTool(callID, func(t *StreamTool) { t.Detail = detail })
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
// role name or a provider/model id, which a workflow may set per run), then
// the agents role.
func (h *Host) subagentModelID(def AgentDef) string {
	return cmp.Or(h.modelRef(def.Model), h.roleModel("agents"))
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
// ones it does. A role name always passes: an unset role follows chat.
func (h *Host) checkModelID(id string) error {
	if id == "" || isRole(id) || slices.Contains(h.ModelIDs(), id) {
		return nil
	}
	return fmt.Errorf("unknown model %q; roles: %s; configured models: %s", id, strings.Join(roleNames(), ", "), strings.Join(h.ModelIDs(), ", "))
}

// ---------------------------------------------------------------------------
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
	if h.webOff() { // it would have only the read tools
		defs = slices.DeleteFunc(defs, func(d AgentDef) bool { return d.Name == "web" && d.Source == lua.OriginBuiltin })
	}
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

func (t *agentTool) Info() fantasy.ToolInfo {
	var b strings.Builder
	b.WriteString("Start a subagent on a self-contained task. It cannot see this conversation, so the prompt carries everything it needs," +
		" what to report and how to verify. Only its final report comes back, and the user does not see it: relay what matters." +
		" Calls sent together run at once.")
	// a detached run outlives a headless turn, which exits as soon as the
	// main agent is idle, so its report would be thrown away
	if t.h.hooks() != nil {
		b.WriteString(" Set background for long work you need not wait on. send_to continues a run, running or finished, by its run id.")
	}
	b.WriteString("\n\nAgents:")
	for _, d := range t.defs {
		fmt.Fprintf(&b, "\n- %s: %s", d.Name, truncRunes(d.Description, 200))
		if d.Model != "" {
			fmt.Fprintf(&b, " (model %s)", d.Model)
		}
	}
	// headless there is no send_to, so the two fields it qualifies are plain required
	required := "; required"
	if t.h.hooks() != nil {
		required = "; required unless send_to is set"
	}
	params := map[string]any{
		"description": map[string]any{"type": "string", "description": "3-5 word label shown to the user" + required},
		"prompt":      map[string]any{"type": "string", "description": "the full task"},
		"agent":       map[string]any{"type": "string", "enum": t.names(), "description": "explore for reading and searching, general only when it must run commands or edit" + required},
	}
	if t.h.hooks() != nil {
		params["background"] = map[string]any{"type": "boolean", "description": "run detached; the report arrives later as a message"}
		params["send_to"] = map[string]any{"type": "string", "description": "id of a run to continue, a workflow's agent too: a running one reads prompt at its next step, a finished one answers it with its history kept; agent and description are then ignored"}
	}
	return fantasy.ToolInfo{
		Name:        tools.ToolAgent,
		Description: b.String(),
		Parameters:  params,
		Required:    []string{"prompt"},
		Parallel:    true,
	}
}

// bgAgentStarted opens the result of an agent call that went on in the
// background, whose row reads as still running (toolStatus).
const bgAgentStarted = "Started background agent "

func (t *agentTool) Run(ctx context.Context, call fantasy.ToolCall) (reply fantasy.ToolResponse, err error) {
	var in struct {
		Description string `json:"description"`
		Prompt      string `json:"prompt"`
		Agent       string `json:"agent"`
		Background  bool   `json:"background"`
		SendTo      string `json:"send_to"`
	}
	if err := decodeInput(t.Info(), call.Input, &in); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return fantasy.NewTextErrorResponse("prompt is required"), nil
	}
	if resp, stop := t.h.preCheck("agent", call.Input); stop {
		return resp, nil
	}
	defer t.h.postNotify(ctx, t.pipe.Pad, "agent", call.Input, &reply, time.Now())
	if in.SendTo != "" {
		run := t.h.tasks.find(in.SendTo)
		if run == nil {
			var err error
			if run, err = t.h.loadRun(in.SendTo); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
		}
		// its child session and bill belong to the conversation that started it
		if run.parent != t.h.AgentSessionID() {
			return fantasy.NewTextErrorResponse("agent " + run.id + " belongs to another session: start a fresh run"), nil
		}
		if run.isRunning() {
			if err := t.h.MessageAgent(run.id, in.Prompt); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			t.h.setToolDetail(call.ID, "message to "+run.id)
			return fantasy.NewTextResponse("Queued for agent " + run.id + ": it reads the message at its next step. Its report still arrives when it finishes."), nil
		}
		t.h.setTool(call.ID, func(st *StreamTool) { st.Run = run.id })
		return t.foreground(ctx, call.ID, run, func(ctx context.Context, progress func(string)) (string, error) {
			return t.h.continueRun(ctx, run, in.Prompt, progress)
		})
	}
	if strings.TrimSpace(in.Agent) == "" {
		// only send_to may leave it out; an omission must not silently run
		// the agent with the most tools
		return fantasy.NewTextErrorResponse("agent is required: " + strings.Join(t.names(), ", ")), nil
	}
	def, err := t.h.agentDef(in.Agent)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	in.Description = cmp.Or(strings.TrimSpace(in.Description), def.Name)
	if in.Background && t.h.hooks() != nil {
		id, err := t.h.startBackgroundAgent(def, in.Description, in.Prompt)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		t.h.setTool(call.ID, func(st *StreamTool) { st.Detail, st.Run = def.Name+" · background "+id+" (/agents)", id })
		return fantasy.NewTextResponse(bgAgentStarted + id + ". Its report will arrive as a message when it finishes; do not wait for it."), nil
	}
	run, rctx := t.h.newRun(ctx, def, in.Description, in.Prompt, false)
	_ = t.h.tasks.add(run)
	t.h.setTool(call.ID, func(st *StreamTool) { st.Run = run.id })
	return t.foreground(rctx, call.ID, run, func(ctx context.Context, progress func(string)) (string, error) {
		return t.h.runAgent(ctx, run, def, in.Prompt, progress)
	})
}

// foreground waits for a run and answers with its report, the row's
// detail following its progress; the run id heads the report (the tail
// may be the spill file's name) so the model can continue the run.
func (t *agentTool) foreground(ctx context.Context, callID string, run *agentRun, do func(context.Context, func(string)) (string, error)) (fantasy.ToolResponse, error) {
	start := time.Now()
	var last atomic.Value
	text, err := do(ctx, func(line string) {
		last.Store(line)
		t.h.setToolDetail(callID, line)
	})
	summary, _ := last.Load().(string)
	if summary == "" {
		summary = run.agent
	}
	t.h.setToolDetail(callID, summary+" · "+fmtDuration(time.Since(start)))
	if err != nil {
		// the chat draws a failure's output, so the id rides its first line rather than a [run] head
		msg := "agent " + run.id + " failed: " + t.h.friendlyError(err)
		if errors.Is(err, context.Canceled) {
			// only the user cancels a run: /agents stop, or esc
			msg = "agent " + run.id + ": the user stopped this agent; do not start it again unless they ask"
		}
		if text != "" {
			msg += "\n\nIts last message before that:\n" + text
		}
		return fantasy.NewTextErrorResponse(msg), nil
	}
	return fantasy.NewTextResponse("[run " + run.id + "]\n\n" + t.h.truncOutput(t.pipe.Pad, "agent", text)), nil
}

// decodeInput validates a host tool's input against its schema, so a wrong
// type is worded as it is for a builtin tool, then decodes it into in.
func decodeInput(info fantasy.ToolInfo, input string, in any) error {
	if _, err := tools.ParseInput(info, input, false); err != nil {
		return err
	}
	return json.Unmarshal([]byte(input), in)
}

// preCheck runs the tool.pre events for a host-implemented tool: deny
// becomes an error result, halt stops the turn. A rewrite is refused: these
// tools run and gate their own input, so running the original would ignore it.
func (h *Host) preCheck(tool, input string) (fantasy.ToolResponse, bool) {
	var m map[string]any
	_ = json.Unmarshal([]byte(input), &m)
	decision, reason, _, _ := h.toolPreHook()(tool, m)
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

// ---------------------------------------------------------------------------
// /agents and zemi.agent.run

// newAgentPrompt rides behind /agents new and takes zemiSkillDir, where the
// procedure lives.
const newAgentPrompt = "I want a new zemi subagent that does what the command above says; ask me when it says nothing. Read SKILL.md in %s, the zemi skill's directory, then follow the New agent steps in agents.md there."

// agentsCommands is /agents.
func (h *Host) agentsCommands() []*Command {
	return []*Command{{
		Name: "agents", NArgs: NArgsAny, Desc: "see running agents and the ones zemi can start, or make one",
		GoFn: func(h *Host, args string) error {
			typed, stdin := agent.CutAttached(args)
			sub, id, _ := strings.Cut(strings.TrimSpace(typed), " ")
			switch id = strings.TrimSpace(id); sub {
			case "":
				return h.openAgents()
			case "new":
				// the line as typed, newlines included, with the prompt behind it
				return h.AgentSend("/agents " + strings.TrimSpace(typed) + agent.Attached("command", fmt.Sprintf(newAgentPrompt, h.zemiSkillDir())) + stdin)
			case "open":
				return h.openAgentFile(id)
			case "view":
				return h.ViewAgent(id)
			case "stop":
				return h.stopRun(id)
			case "kill":
				return h.jobs.Kill(id)
			}
			return Usagef("unknown /agents subcommand %q (new [what it does], open <name>, view <id>, stop [id], kill <job id>)", sub)
		},
		GoComplete: SubcommandCompleter([]string{"new", "open", "view", "stop", "kill"}, func(h *Host, sub string) []string {
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
	defs := h.agentDefs()
	runs := h.agentRunItems()
	sel := 0
	if len(runs) > 0 && runs[0].(map[string]any)["section"] == "Running" { // runs list running first
		sel = 1
	}
	items := append([]any{map[string]any{"text": "New agent", "verb": "fill", "fill": "/agents new "}}, runs...)
	// jobs belong to the host, so a server started in an earlier session shows too
	for _, j := range h.jobs.List() {
		item := map[string]any{
			"text":    j.ID,
			"desc":    j.Status() + " · " + j.Name,
			"section": "Background jobs (all sessions)",
			"fields":  fieldList("name", j.Name, "status", j.Status()),
			"mark":    j.Mark(),
			"detail":  cmp.Or(shortOutput(j.Output()), "(no output)"),
		}
		if j.Running() {
			item["actions"] = []any{map[string]any{"text": "Stop", "hotkey": "s", "run": "agents kill " + j.ID}}
		}
		items = append(items, item)
	}
	for _, d := range defs {
		model, tools, access := cmp.Or(d.Model, "agents"), "every tool a subagent may use", "can change files"
		if d.Tools != nil {
			tools = strings.Join(d.Tools, ", ")
		}
		if d.ReadOnly {
			access = "read only"
		}
		item := map[string]any{
			"text":    d.Name,
			"desc":    listDesc(d.Description),
			"section": sourceSection(d.Source) + " agents", // apart from the runs above
			"detail":  d.Description,
		}
		fields := []string{"source", h.sourceOf(d.Source, d.Path), "model", model, "tools", tools, "access", access}
		if d.Path != "" {
			fields = append(fields, "path", perm.TildePath(d.Path))
			item["actions"] = []any{map[string]any{"text": "Open in the editor", "hotkey": "o", "run": "agents open " + d.Name}}
		}
		item["fields"] = fieldList(fields...)
		items = append(items, item)
	}
	ui.OpenOverlay(Overlay{ID: "agents", Title: "Agents", Origin: lua.OriginBuiltin, Items: items, Selected: sel, Verb: "open"})
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
		h.refuseBelowUser(L, "agent.run")
		prompt := rawString(L, spec, "prompt")
		if strings.TrimSpace(prompt) == "" {
			L.RaiseError("zemi.agent.run: prompt is required")
			return 0
		}
		def, err := h.agentDef(rawString(L, spec, "agent"))
		if err != nil {
			L.RaiseError("zemi.agent.run: %v", err)
			return 0
		}
		desc := rawString(L, spec, "description")
		if desc == "" {
			desc = clipRunes(prompt, 40)
		}
		ctx, cancel := context.WithCancel(context.Background())
		at := h.callerOf(L)
		stopID := h.stops.add(cancel) // a reload cancels it, like a job
		h.GoSafe("zemi.agent.run", func() {
			defer cancel()
			defer h.stops.remove(stopID)
			_, text, err := h.runSubagent(ctx, def, desc, prompt)
			switch {
			case onDone == nil:
			case err != nil:
				h.callLuaAsync("zemi.agent.run on_done", at, onDone, nil, h.friendlyError(err))
			default:
				h.callLuaAsync("zemi.agent.run on_done", at, onDone, text, nil)
			}
		})
		handle := L.NewTable()
		handle.RawSetString("cancel", L.NewFunction(func(L *glua.LState) int {
			cancel()
			return 0
		}))
		lua.GuardFuncs(L, handle, "agent:", h.guardLua)
		L.Push(handle)
		return 1
	}))
}
