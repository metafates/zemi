// agent.go is the M1 agent core in the host (DESIGN §6.3, §13): the
// provider/model/agent/permission/session namespaces, profile resolution,
// turn streaming into the UI, message persistence and the steering queue.
package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
	glua "github.com/yuin/gopher-lua"
)

// Profile is one named agent shape: model, system prompt, tools.
type Profile struct {
	Name         string   `json:"name"`
	Model        string   `json:"model"`
	System       string   `json:"system"`
	Tools        []string `json:"tools"`
	ContextFiles []string `json:"context_files"`
	Temperature  *float64 `json:"temperature,omitempty"`
}

// AgentEvent is one streaming update delivered to the UI (hostapi stays
// UI-agnostic; internal/ui implements the display).
type AgentEvent struct {
	Kind             string // turn_start, text, reasoning, tool_start, tool_end, turn_done
	Text             string
	CallID           string
	Tool             string
	Input            string
	OK               bool
	Err              string
	PromptTokens     int64
	CompletionTokens int64
}

// PermRequest is one interactive permission ask for the overlay.
type PermRequest struct {
	Subject string
	Arg     string
	Reason  string
	Preview string // file tools: the proposed change as "- "/"+ " lines
}

// ModelOption is one picker row.
type ModelOption struct {
	ID      string
	Display string
	HasKey  bool
}

// StreamTool is one tool call in the live stream view (DESIGN §6.3).
type StreamTool struct {
	ID     string
	Detail string // live progress line (subagents, workflows)
	Diff   string // file tools: the change as "- "/"+ " lines
	Name   string
	Input  string
	Status string // "run", "ok", "err"
}

// agentHost owns the live agent session: profiles, the runner, the
// steering queue and the stream view the UI renders.
type agentHost struct {
	mu        sync.Mutex
	profiles  map[string]*Profile
	defProf   string
	runner    *agent.Runner
	sessionID string
	sessionDB *store.DB

	active    bool
	textBuf   string
	reasonBuf string
	tools     []StreamTool
	usage     fantasy.Usage
	steering  []string
	modelSeed string

	// session usage (DESIGN §8 statusline tally): sessIn/sessOut accumulate
	// every turn's counts, lastIn pins the most recent prompt size (the
	// current context estimate), ctxWin overrides the context window when
	// the model spec or config provides one.
	sessIn   int64
	sessOut  int64
	lastIn   int64
	ctxWin   int64
	sessCost float64 // USD, from model spec prices

	// reasoningShown is the session-scoped expansion override for finished
	// reasoning blocks (nil = follow ui.chat.reasoning config).
	reasoningShown *bool

	// turn timing: turnStart pins the wall clock at AgentSend, thinking
	// tracks the open reasoning block, thought accumulates closed ones.
	turnStart  time.Time
	thinkStart time.Time
	thinking   bool
	thought    time.Duration

	// retryAt is when the next provider retry fires; retries counts them
	// this turn (the working row counts down instead of looking stuck).
	retryAt    time.Time
	retries    int
	retryWhy   string // friendlyError of the failure being retried
	compacting bool   // summarizing history before the next turn

	// digest is pinned at session start (DESIGN §10.7) and refreshed only
	// at compaction; memAuto counts auto-applied memories against the
	// session cap (memory proposals persist in the store), cfgProposals
	// parks validated config patches for review, cfgAuto counts
	// auto-applied rules against the rules cap, reflected counts closed
	// sessions for the reflector every-N gate.
	digest       string
	memAuto      int
	cfgProposals []*cfgProposal
	cfgAuto      int
	reflected    int
	reflectErr   string // last reflector run's error, "" after a success (/health)

	// turnMsgID is the stored user message of the running turn; write tools
	// checkpoint files against it (rewind).
	turnMsgID string

	// shellNotes holds !command output waiting for the next message.
	shellNotes []string
}

func newAgentHost() *agentHost {
	return &agentHost{profiles: map[string]*Profile{}}
}

// ---------------------------------------------------------------------------
// host wiring

const contextBudget = 32 << 10

// defaultSystemPrompt is the shipped prompt (no profile configured).
func (h *Host) defaultSystemPrompt() string {
	return "You are zemi, a coding agent working in " + h.Dirs.ProjectDir + "." +
		" Keep replies concise. Use the provided tools to inspect and edit" +
		" files; destructive or unusual actions need a clear reason." +
		" You run inside zemi: when the user asks about zemi itself (its config," +
		" Lua API, errors or what it learned), load the matching zemi-* skill first." +
		delegationPrompt
}

// delegationPrompt tells the main agent when subagents and workflows pay
// off; without it models rarely reach for them unprompted.
const delegationPrompt = `

# Delegating
You can hand work to subagents with the agent tool. Each starts with an empty context, so write a complete prompt: the goal, what you already know, and what to report.
- Use the explore agent for open-ended searches and questions about code ("where is X handled", "how does Y work") that would take many reads, so your context keeps only the answer. Use general only when the subagent must run commands or edit files.
- When a task splits into independent parts (several modules, several questions), launch one agent per part in the same message; they run in parallel.
- When the work has stages with logic between them (fan out reviews then verify findings, migrate files then check each, try alternatives then compare), write a workflow script and run it with the workflow tool.
- Do small, targeted work yourself: a known file, a single grep, a one-line fix. Delegating it only adds latency.
Subagent reports are not shown to the user; summarize what matters.`

// Effort is the configured reasoning effort ("" = provider default).
func (h *Host) Effort() string {
	v, _ := h.Settings.Get("agent.effort")
	s, _ := v.(string)
	return s
}

// InitProject asks the agent to write or refresh AGENTS.md (/init).
func (h *Host) InitProject() error { return h.AgentSend(initPrompt) }

const initPrompt = `Study this repository and write AGENTS.md at its root for future coding agents; if it exists, update it and keep what is still true. Cover, briefly and concretely: what the project is, in two sentences; the exact commands to build, test, lint and run, taken from the build files; the main packages and how control flows between them; conventions and non-obvious gotchas you confirmed in the code. No generic advice. In a large codebase, first explore different areas with parallel explore agents.`

// resolveProfile returns the profile to run: explicit name, the default,
// or the implicit shipped profile.
func (h *Host) resolveProfile(name string) (*Profile, error) {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if name == "" {
		name = ag.defProf
	}
	if p, ok := ag.profiles[name]; ok {
		return p, nil
	}
	if name != "" && name != "default" {
		return nil, fmt.Errorf("agent: unknown profile %q", name)
	}
	return nil, nil
}

// currentModelID picks the model: profile override, config agent.model,
// else the first catalog entry.
func (h *Host) currentModelID(p *Profile) string {
	if p != nil && p.Model != "" {
		return p.Model
	}
	if v, ok := h.Settings.Get("agent.model"); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	for _, spec := range h.Providers.List() {
		for _, m := range spec.Models {
			return spec.ID + "/" + m.ID
		}
	}
	return ""
}

// buildSystem renders the system prompt with context files appended
// (capped; DESIGN §6.3 context.files).
func (h *Host) buildSystem(p *Profile) string {
	sys := h.defaultSystemPrompt()
	var files []string
	if p != nil {
		if p.System != "" {
			sys = p.System
		}
		files = p.ContextFiles
	}
	if h.PermMode() == "plan" {
		sys += planPrompt
	}
	return h.systemWith(sys, files)
}

// systemWith appends instruction files, the skills index and the session
// digest to a base prompt. files names explicit project files (a profile's
// context.files); nil means instructionFiles.
func (h *Host) systemWith(sys string, files []string) string {
	if len(files) == 0 {
		files = h.instructionFiles()
	} else {
		for i, f := range files {
			files[i] = filepath.Join(h.Dirs.ProjectDir, f)
		}
	}
	budget := contextBudget
	var sections string
	for _, f := range files {
		if budget <= 0 {
			break
		}
		b, err := os.ReadFile(f)
		if err != nil || len(b) == 0 {
			continue
		}
		if len(b) > budget {
			b = b[:budget]
		}
		budget -= len(b)
		name := f
		if rel, err := filepath.Rel(h.Dirs.RepoRoot(), f); err == nil && !strings.HasPrefix(rel, "..") {
			name = rel
		}
		sections += "\n\n## Instructions: " + name + "\n" + string(b)
	}
	return h.withDigest(sys + sections + h.assembleSections() + h.skillsIndexPrompt())
}

// instructionFiles lists the instruction files for this project, general
// first: the user's <config>/AGENTS.md, then for each directory from the
// repository root down to the working directory its ZEMI.md and AGENTS.md,
// or CLAUDE.md when it has neither (repositories set up for Claude Code).
func (h *Host) instructionFiles() []string {
	var out []string
	if h.Dirs.ConfigHome != "" {
		out = append(out, filepath.Join(h.Dirs.ConfigHome, "AGENTS.md"))
	}
	var dirs []string
	root := h.Dirs.RepoRoot()
	for dir := h.Dirs.ProjectDir; dir != ""; dir = filepath.Dir(dir) {
		dirs = append([]string{dir}, dirs...)
		if dir == root || filepath.Dir(dir) == dir {
			break
		}
	}
	for _, dir := range dirs {
		found := false
		for _, name := range []string{"ZEMI.md", "AGENTS.md"} {
			if p := filepath.Join(dir, name); fileExists(p) {
				out, found = append(out, p), true
			}
		}
		if p := filepath.Join(dir, "CLAUDE.md"); !found && fileExists(p) {
			out = append(out, p)
		}
	}
	return out
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// withDigest appends the pinned session digest behind the project context
// (DESIGN §10.7): the digest is computed once at session creation and
// refreshed only at compaction, keeping provider prompt caches warm.
func (h *Host) withDigest(sys string) string {
	ag := h.agents
	ag.mu.Lock()
	digest := ag.digest
	ag.mu.Unlock()
	if digest == "" {
		return sys
	}
	return sys + "\n\n" + digest
}

// profileTools selects the tool set: profile names filter the builtin set;
// Lua-registered tools are always included (the pipeline gates them).
func (h *Host) profileTools(p *Profile) []fantasy.AgentTool {
	var want []string
	if p != nil && len(p.Tools) > 0 {
		want = p.Tools
	}
	pipe := h.PipelineFor()
	out := filterTools(append(tools.All(pipe), h.agentTool(), h.workflowTool()), want, nil)
	out = append(out, h.luaFantasyTools()...)
	return append(out, h.mcpFantasyTools(pipe)...)
}

// resolveModel picks the agent model for a profile (tests stub modelFn).
func (h *Host) resolveModel(p *Profile) (fantasy.LanguageModel, error) {
	if h.modelFn != nil {
		return h.modelFn()
	}
	id := h.currentModelID(p)
	if id == "" {
		return nil, fmt.Errorf("agent: no models registered")
	}
	return h.Providers.Resolve(id)
}

// ensureRunner builds the runner for the current profile and session.
func (h *Host) ensureRunner(p *Profile) (*agent.Runner, error) {
	model, err := h.resolveModel(p)
	if err != nil {
		return nil, err
	}
	r := agent.NewRunner(model, h.profileTools(p), h.buildSystem(p))
	r.ProviderOptions = h.Providers.EffortOptions(h.currentModelID(p), h.Effort())
	r.Hooks = h.streamHooks()
	r.Persist = h.persistMessage
	r.Steer = h.deliverSteering
	return r, nil
}

// AgentSend sends one user message to the agent (DESIGN §4, §6.3). Idle,
// it starts a turn; during a turn it queues, and the queue joins the
// running turn at its next step boundary. Safe from any goroutine.
func (h *Host) AgentSend(text string) error {
	ag := h.agents
	ag.mu.Lock()
	if ag.active {
		ag.steering = append(ag.steering, text)
		ag.mu.Unlock()
		h.refresh()
		return nil
	}
	profName := ag.defProf
	ag.mu.Unlock()

	p, err := h.resolveProfile(profName)
	if err != nil {
		return err
	}
	if err := h.ensureSession(p, text); err != nil {
		return err
	}
	runner, err := h.ensureRunner(p)
	if err != nil {
		return err
	}

	ag.mu.Lock()
	// The runner is rebuilt per send (model, prompt and tools may have
	// changed); the conversation carries over from the idle previous one.
	if prev := ag.runner; prev != nil {
		runner.History = prev.History
	}
	ag.runner = runner
	ag.active = true
	ag.modelSeed = ""
	ag.beginTurn()
	ag.mu.Unlock()
	h.startTurn(text)

	h.GoSafe("agent turn", func() { h.runTurns(runner, text) })
	return nil
}

// runTurns sends text and every follow-up the queue produces on one
// runner, compacting first whenever the last prompt filled the context.
func (h *Host) runTurns(runner *agent.Runner, text string) {
	defer h.endTurnAfterPanic()
	for text != "" {
		if h.compactDue() {
			h.compactReporting(runner)
		}
		if err := runner.Send(context.Background(), h.expandAttachments(text)); err != nil && !errors.Is(err, agent.ErrTurnActive) {
			h.Log.Error("agent", "turn failed", map[string]any{"error": err.Error()})
		}
		text = h.nextTurn()
	}
}

func (h *Host) startTurn(text string) {
	// what follows attachMarker is for the model (background reports), the
	// same cut resumed transcripts make
	shown, _, _ := strings.Cut(text, attachMarker)
	h.AppendMessage("user", shown)
	h.agentEvent(AgentEvent{Kind: "turn_start"})
	h.EmitAsync("agent.start", map[string]any{"text": text})
}

// nextTurn runs after each turn: messages queued after the last step
// boundary become one follow-up turn, otherwise the agent goes idle.
// active stays held across the handoff, so a submit in between queues
// instead of starting a turn out of order.
func (h *Host) nextTurn() string {
	ag := h.agents
	ag.mu.Lock()
	text := strings.Join(ag.steering, "\n\n")
	ag.steering = nil
	if text == "" {
		ag.active = false
	} else {
		ag.beginTurn()
	}
	ag.mu.Unlock()
	if text == "" {
		h.refresh()
	} else {
		h.startTurn(text)
	}
	return text
}

// deliverSteering hands the queue to the running turn at a step boundary.
// The reply streamed so far moves into the transcript first, so each
// steered message shows after the text it interrupted.
func (h *Host) deliverSteering() []string {
	ag := h.agents
	ag.mu.Lock()
	msgs, text, reason, tools := ag.steering, ag.textBuf, ag.reasonBuf, ag.tools
	ag.endThinking()
	stat := TurnStat{ThoughtMS: ag.thought.Milliseconds()}
	if len(msgs) > 0 {
		ag.steering, ag.textBuf, ag.reasonBuf, ag.tools, ag.thought = nil, "", "", nil, 0
	}
	ag.mu.Unlock()
	if len(msgs) == 0 {
		return nil
	}
	h.appendTurnOutput(reason, stat, tools, text, TurnStat{})
	out := make([]string, len(msgs))
	for i, m := range msgs {
		shown, _, _ := strings.Cut(m, attachMarker)
		h.AppendMessage("user", shown)
		out[i] = h.expandAttachments(m)
	}
	return out
}

// AgentTakeQueue removes and returns the messages still waiting for a step
// boundary, so the UI can hand them back to the composer.
func (h *Host) AgentTakeQueue() []string {
	ag := h.agents
	ag.mu.Lock()
	q := ag.steering
	ag.steering = nil
	ag.mu.Unlock()
	if len(q) > 0 {
		h.refresh()
	}
	return q
}

// beginTurn resets the per-turn stream state (ag.mu held).
func (ag *agentHost) beginTurn() {
	ag.textBuf, ag.reasonBuf = "", ""
	ag.tools = nil
	ag.turnStart = time.Now()
	ag.thinkStart, ag.thinking, ag.thought = time.Time{}, false, 0
	ag.retryAt, ag.retries, ag.retryWhy = time.Time{}, 0, ""
}

// AgentCancel aborts the active turn (DESIGN §6.3).
func (h *Host) AgentCancel() {
	ag := h.agents
	ag.mu.Lock()
	r := ag.runner
	ag.mu.Unlock()
	if r != nil {
		r.Cancel()
	}
}

// AgentActive reports whether a turn is running.
func (h *Host) AgentActive() bool {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return ag.active
}

// agentEvent delivers one streaming update to the UI.
func (h *Host) agentEvent(ev AgentEvent) {
	if ui := h.hooks(); ui != nil {
		ui.AgentEvent(ev)
	}
}

// streamHooks adapts agent.StreamHooks into UI events and stream state.
func (h *Host) streamHooks() agent.StreamHooks {
	return &streamAdapter{h: h}
}

type streamAdapter struct{ h *Host }

func (s *streamAdapter) TextDelta(text string) {
	ag := s.h.agents
	ag.mu.Lock()
	ag.endThinking()
	ag.textBuf += text
	ag.mu.Unlock()
	s.h.agentEvent(AgentEvent{Kind: "text", Text: text})
	// NOTE: one queue round trip per delta; fine for the few configs that
	// listen, coalesce here if one ever streams into something slow.
	if len(s.h.Events.Handlers("agent.text_delta")) > 0 {
		s.h.EmitAsync("agent.text_delta", map[string]any{"text": text})
	}
}

func (s *streamAdapter) Retrying(err error, delay time.Duration) {
	ag := s.h.agents
	ag.mu.Lock()
	ag.retries++
	ag.retryAt = time.Now().Add(delay)
	// fantasy passes no error when a network failure (DNS, refused, reset)
	// caused the retry.
	ag.retryWhy = "network problem"
	if err != nil {
		ag.retryWhy = friendlyError(err)
	}
	ag.mu.Unlock()
	s.h.Log.Warn("agent", "provider retry", map[string]any{"error": errString(err), "delay_ms": delay.Milliseconds()})
	s.h.agentEvent(AgentEvent{Kind: "retry"})
}

func (s *streamAdapter) ReasoningDelta(text string) {
	ag := s.h.agents
	ag.mu.Lock()
	ag.beginThinking()
	ag.reasonBuf += text
	ag.mu.Unlock()
	s.h.agentEvent(AgentEvent{Kind: "reasoning", Text: text})
	if len(s.h.Events.Handlers("agent.reasoning_delta")) > 0 {
		s.h.EmitAsync("agent.reasoning_delta", map[string]any{"text": text})
	}
}

func (s *streamAdapter) ToolCallStart(id, name, input string) {
	ag := s.h.agents
	ag.mu.Lock()
	ag.endThinking()
	diff := ""
	if name == "write" || name == "edit" || name == "multiedit" {
		var in map[string]any
		if json.Unmarshal([]byte(input), &in) == nil {
			diff = capLines(tools.ChangePreview(in), 8)
		}
	}
	// Text streamed before this call belongs above it: move it and the
	// earlier (finished) tool rows into the transcript, in order, so the
	// next step's text starts its own block instead of running on.
	reason, tools, text := ag.reasonBuf, ag.tools, ag.textBuf
	stat := TurnStat{ThoughtMS: ag.thought.Milliseconds()}
	flush := strings.TrimSpace(text) != ""
	if flush {
		ag.reasonBuf, ag.tools, ag.textBuf, ag.thought = "", nil, "", 0
	}
	ag.tools = append(ag.tools, StreamTool{ID: id, Name: name, Input: toolSummary(input), Status: "run", Diff: diff})
	ag.mu.Unlock()
	if flush {
		s.h.appendTurnOutput(reason, stat, tools, text, TurnStat{})
	}
	s.h.agentEvent(AgentEvent{Kind: "tool_start", CallID: id, Tool: name, Input: input})
}

func (s *streamAdapter) ToolCallEnd(id, name string, ok bool, output string) {
	ag := s.h.agents
	ag.mu.Lock()
	for i := len(ag.tools) - 1; i >= 0; i-- {
		if ag.tools[i].ID == id && ag.tools[i].Status == "run" {
			ag.tools[i].Status = map[bool]string{true: "ok", false: "err"}[ok]
			if !ok {
				ag.tools[i].Diff = "" // the change did not happen
			}
			break
		}
	}
	ag.mu.Unlock()
	s.h.agentEvent(AgentEvent{Kind: "tool_end", CallID: id, Tool: name, OK: ok, Input: output})
	s.h.recordToolCall(name, ok, output)
}

func (s *streamAdapter) TurnDone(err error, usage fantasy.Usage) {
	h := s.h
	ag := h.agents
	cost := h.turnCost(h.currentModelID(nil), usage.InputTokens, usage.OutputTokens)
	ag.mu.Lock()
	ag.endThinking()
	ag.usage = usage
	ag.sessCost += cost
	ag.sessIn += usage.InputTokens
	ag.sessOut += usage.OutputTokens
	ag.lastIn = usage.InputTokens
	text, reason, tools := ag.textBuf, ag.reasonBuf, ag.tools
	stat := TurnStat{
		In:        usage.InputTokens,
		Out:       usage.OutputTokens,
		ThoughtMS: ag.thought.Milliseconds(),
	}
	if !ag.turnStart.IsZero() {
		stat.ElapsedMS = time.Since(ag.turnStart).Milliseconds()
	}
	ag.turnStart, ag.thinkStart = time.Time{}, time.Time{}
	ag.thinking, ag.thought = false, 0
	ag.textBuf, ag.reasonBuf = "", ""
	ag.tools = nil
	sid := ag.sessionID
	ag.mu.Unlock()

	h.agentEvent(AgentEvent{
		Kind: "turn_done", Err: errString(err),
		PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens,
	})
	h.appendTurnOutput(reason, TurnStat{ThoughtMS: stat.ThoughtMS, ElapsedMS: stat.ElapsedMS}, tools, text, stat)
	if err != nil {
		h.EmitAsync("agent.error", map[string]any{"error": errString(err)})
		// The failure stays in the transcript: a toast expires, and a turn
		// that ended silently looks like zemi ignored the message.
		row := ChatMessage{Role: "assistant", Kind: "error", Text: truncRunes(sanitize.String(errorRowText(err)), 400)}
		if errors.Is(err, context.Canceled) {
			row.Status = "cancelled"
		}
		h.appendChatMessage(row)
	} else {
		h.EmitAsync("agent.finish", map[string]any{})
	}
	if sid != "" && h.DB != nil {
		_ = h.DB.AddUsage(sid, usage.InputTokens, usage.OutputTokens, cost)
	}
}

// appendTurnOutput moves one stretch of turn output into the transcript in
// display order: the collapsible reasoning block (§8), the tool rows, then
// the reply. A tool still running here was cut off by a cancel.
func (h *Host) appendTurnOutput(reason string, reasonStat TurnStat, tools []StreamTool, text string, textStat TurnStat) {
	if strings.TrimSpace(reason) != "" {
		h.AppendReasoning(reason, reasonStat)
	}
	for _, t := range tools {
		status := t.Status
		if status == "run" {
			status = "err"
		}
		if status != "ok" {
			t.Diff = ""
		}
		h.appendChatMessage(ChatMessage{Role: "assistant", Kind: "tool", Tool: t.Name, Text: t.Input, Status: status, Detail: t.Detail, Diff: t.Diff})
	}
	if strings.TrimSpace(text) != "" {
		h.AppendTurnResult(text, textStat)
	}
}

// capLines keeps the first n lines and notes how many were cut.
func capLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n... %d more lines", len(lines)-n)
}

// toolSummary picks the argument a person reads first (a command, path or
// query) out of a tool's JSON input; anything else stays raw.
func toolSummary(input string) string {
	var m map[string]any
	if json.Unmarshal([]byte(input), &m) != nil {
		return input
	}
	if list, ok := m["todos"].([]any); ok {
		done := 0
		for _, td := range list {
			if t, ok := td.(map[string]any); ok && t["status"] == "done" {
				done++
			}
		}
		return fmt.Sprintf("%d todos, %d done", len(list), done)
	}
	for _, k := range []string{"command", "path", "file_path", "pattern", "url", "query", "question", "name", "text", "description"} {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return input
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// persistMessage writes one fantasy message into the project store.
func (h *Host) persistMessage(role string, msg fantasy.Message) error {
	if h.DB == nil {
		return nil
	}
	ag := h.agents
	ag.mu.Lock()
	sid := ag.sessionID
	ag.mu.Unlock()
	if sid == "" {
		return nil
	}
	parts, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	row := &store.Message{SessionID: sid, Role: role, PartsJSON: string(parts)}
	if err := h.DB.AddMessage(row); err != nil {
		return err
	}
	if role == "user" {
		ag.mu.Lock()
		ag.turnMsgID = row.ID
		ag.mu.Unlock()
	}
	return nil
}

// recordToolCall stores one tool telemetry row (each tool call gets its
// own "tool" message row so tool_calls.message_id stays a real FK).
func (h *Host) recordToolCall(name string, ok bool, output string) {
	if h.DB == nil {
		return
	}
	ag := h.agents
	ag.mu.Lock()
	sid := ag.sessionID
	ag.mu.Unlock()
	if sid == "" {
		return
	}
	parts, _ := json.Marshal(map[string]any{"tool": name, "output": output})
	msg := &store.Message{SessionID: sid, Role: "tool", PartsJSON: string(parts)}
	if err := h.DB.AddMessage(msg); err != nil {
		h.Log.Warn("agent", "tool message persist failed", map[string]any{"error": err.Error()})
		return
	}
	tc := &store.ToolCall{MessageID: msg.ID, Tool: name, InputJSON: "{}", OK: ok}
	if !ok {
		tc.ErrorKind = "error"
	}
	if err := h.DB.AddToolCall(tc); err != nil {
		h.Log.Warn("agent", "tool call persist failed", map[string]any{"error": err.Error()})
	}
}

// ensureSession creates the store session on the first send. Rolling over
// a previous session is the reflector's session-close trigger (DESIGN
// §10.3).
func (h *Host) ensureSession(p *Profile, firstText string) error {
	ag := h.agents
	ag.mu.Lock()
	if ag.sessionID != "" {
		ag.mu.Unlock()
		return nil
	}
	if h.DB == nil {
		ag.mu.Unlock()
		return fmt.Errorf("agent: session store unavailable")
	}
	sid := store.NewID()
	name := ""
	if p != nil {
		name = p.Name
	}
	title := strings.SplitN(strings.TrimSpace(firstText), "\n", 2)[0]
	if r := []rune(title); len(r) > 60 {
		title = string(r[:60])
	}
	if err := h.DB.CreateSession(sid, "", title, name); err != nil {
		ag.mu.Unlock()
		return err
	}
	ag.sessionID = sid
	h.recentStale.Store(true)
	ag.digest = h.digestFor()
	ag.memAuto = 0
	ag.cfgAuto = 0
	ag.mu.Unlock()
	h.EmitAsync("session.new", map[string]any{"id": sid, "title": title})
	return nil
}

// AgentSessionID returns the live session id ("" before the first send).
func (h *Host) AgentSessionID() string {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return ag.sessionID
}

// StreamSnapshot is the live stream state for widgets.
type StreamSnapshot struct {
	Active    bool
	Text      string
	Reasoning string
	Tools     []StreamTool
	Model     string
	Profile   string
	Steering  []string      // queued messages, oldest first
	Thought   time.Duration // wall time spent in reasoning blocks
	Elapsed   time.Duration // whole-turn wall time (0 outside a turn)
	RetryIn   time.Duration // time until the next provider retry (0 when none waits)
	Retries   int           // provider retries this turn
	RetryWhy  string        // what failed, for the retry countdown
	Compacting bool         // history is being summarized before the turn

	// usage tally (DESIGN §8 statusline): session totals, the last prompt
	// size (the current-context estimate), and the context window used for
	// the percent readout.
	SessionIn   int64
	SessionOut  int64
	LastIn      int64
	CtxWindow   int64
	SessionCost float64
}

// AgentStream returns the current stream snapshot.
func (h *Host) AgentStream() StreamSnapshot {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	p := ag.defProf
	if pr, ok := ag.profiles[p]; ok {
		p = pr.Model
	}
	thought := ag.thought
	if ag.thinking {
		thought += time.Since(ag.thinkStart)
	}
	var elapsed time.Duration
	if ag.active && !ag.turnStart.IsZero() {
		elapsed = time.Since(ag.turnStart)
	}
	out := StreamSnapshot{
		Active: ag.active, Text: ag.textBuf, Reasoning: ag.reasonBuf,
		Profile: ag.defProf, Steering: append([]string(nil), ag.steering...),
		Thought: thought, Elapsed: elapsed,
		SessionIn: ag.sessIn, SessionOut: ag.sessOut, LastIn: ag.lastIn,
		CtxWindow: ag.ctxWin,
	}
	out.Model = ag.modelSeed
	if out.Model == "" {
		out.Model = h.currentModelID(nil)
	}
	if out.CtxWindow == 0 {
		out.CtxWindow = h.contextWindow(out.Model)
	}
	out.Tools = append(out.Tools, ag.tools...)
	out.Retries, out.Compacting, out.SessionCost = ag.retries, ag.compacting, ag.sessCost
	if d := time.Until(ag.retryAt); d > 0 {
		out.RetryIn, out.RetryWhy = d, ag.retryWhy
	}
	return out
}

// SeedStream installs a stream snapshot directly (no turn runs). It backs
// golden tests and UI development: the next AgentStream read returns the
// seeded state until a real turn resets it.
func (h *Host) SeedStream(s StreamSnapshot) {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	ag.active = s.Active
	ag.textBuf = s.Text
	ag.reasonBuf = s.Reasoning
	ag.tools = s.Tools
	ag.modelSeed = s.Model
	ag.thought = s.Thought
	ag.thinking = false
	ag.sessIn = s.SessionIn
	ag.sessOut = s.SessionOut
	ag.lastIn = s.LastIn
	ag.ctxWin = s.CtxWindow
	if s.Elapsed > 0 && s.Active {
		ag.turnStart = time.Now().Add(-s.Elapsed)
	} else {
		ag.turnStart = time.Time{}
	}
	ag.steering = append([]string(nil), s.Steering...)
	ag.retries, ag.retryAt = s.Retries, time.Time{}
	if s.RetryIn > 0 {
		ag.retryAt = time.Now().Add(s.RetryIn)
	}
}

// beginThinking opens a reasoning block (ag.mu held); endThinking closes
// any open block into the accumulated thought time.
func (ag *agentHost) beginThinking() {
	if !ag.thinking {
		ag.thinking = true
		ag.thinkStart = time.Now()
	}
}

func (ag *agentHost) endThinking() {
	if ag.thinking {
		ag.thought += time.Since(ag.thinkStart)
		ag.thinking = false
	}
}

// ---------------------------------------------------------------------------
// namespaces

func (h *Host) providerNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("register", L.NewFunction(func(L *glua.LState) int {
		if h.RT.CurrentOrigin() != lua.OriginUser {
			L.RaiseError("zemi.provider.register: only user config may register providers")
			return 0
		}
		spec, err := providerSpecFromLua(L, 1)
		if err != nil {
			L.RaiseError("zemi.provider.register: %v", err)
			return 0
		}
		if err := h.Providers.Register(spec); err != nil {
			L.RaiseError("zemi.provider.register: %v", err)
			return 0
		}
		return 0
	}))
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		for _, s := range h.Providers.List() {
			row := L.NewTable()
			row.RawSetString("id", glua.LString(s.ID))
			row.RawSetString("kind", glua.LString(s.Kind))
			row.RawSetString("base_url", glua.LString(s.BaseURL))
			out.Append(row)
		}
		L.Push(out)
		return 1
	}))
	return tb
}

func providerSpecFromLua(L *glua.LState, idx int) (agent.Spec, error) {
	v := L.Get(idx)
	tb, ok := v.(*glua.LTable)
	if !ok {
		return agent.Spec{}, fmt.Errorf("spec must be a table")
	}
	spec := agent.Spec{
		ID:        rawString(tb, "id"),
		Kind:      rawString(tb, "kind"),
		BaseURL:   rawString(tb, "base_url"),
		APIKeyEnv: rawString(tb, "api_key_env"),
	}
	models, ok := tb.RawGetString("models").(*glua.LTable)
	if ok {
		models.ForEach(func(_, mv glua.LValue) {
			if mt, ok := mv.(*glua.LTable); ok {
				m := agent.Model{
					ID:      rawString(mt, "id"),
					Display: rawString(mt, "display"),
				}
				if cw, ok := mt.RawGetString("context_window").(glua.LNumber); ok && cw > 0 {
					m.ContextWindow = int64(cw)
				}
				if c, ok := mt.RawGetString("cost_per_1m_in").(glua.LNumber); ok && c > 0 {
					m.CostPer1MIn = float64(c)
				}
				if c, ok := mt.RawGetString("cost_per_1m_out").(glua.LNumber); ok && c > 0 {
					m.CostPer1MOut = float64(c)
				}
				spec.Models = append(spec.Models, m)
			}
		})
	}
	if spec.ID == "" || spec.Kind == "" {
		return agent.Spec{}, fmt.Errorf("provider needs id and kind")
	}
	return spec, nil
}

func (h *Host) modelNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		for _, s := range h.Providers.List() {
			for _, m := range s.Models {
				row := L.NewTable()
				row.RawSetString("id", glua.LString(s.ID+"/"+m.ID))
				row.RawSetString("display", glua.LString(m.Display))
				row.RawSetString("has_key", glua.LBool(h.Providers.HasKey(s.ID)))
				out.Append(row)
			}
		}
		L.Push(out)
		return 1
	}))
	tb.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		id := L.CheckString(1)
		if !h.modelExists(id) {
			L.RaiseError("zemi.model.set: unknown model %q", id)
			return 0
		}
		if _, err := h.Settings.SetQuiet("agent.model", id); err != nil {
			L.RaiseError("zemi.model.set: %v", err)
			return 0
		}
		h.deliverFired(L, nil)
		return 0
	}))
	tb.RawSetString("pick", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.model.pick") {
			return 0
		}
		id, ok := h.pickModel()
		if !ok {
			L.Push(glua.LNil)
			return 1
		}
		if _, err := h.Settings.SetQuiet("agent.model", id); err != nil {
			L.RaiseError("zemi.model.pick: %v", err)
			return 0
		}
		L.Push(glua.LString(id))
		return 1
	}))
	return tb
}

// contextWindow resolves the context window for the model id: the model
// spec's context_window, then the agent.context_window config knob, then
// the 200k default.
// turnCost prices one turn's tokens from the model spec; 0 when the spec
// carries no prices.
func (h *Host) turnCost(modelID string, in, out int64) float64 {
	prov, model, ok := strings.Cut(modelID, "/")
	if !ok {
		return 0
	}
	spec, ok := h.Providers.Lookup(prov)
	if !ok {
		return 0
	}
	for _, m := range spec.Models {
		if m.ID == model {
			return (float64(in)*m.CostPer1MIn + float64(out)*m.CostPer1MOut) / 1e6
		}
	}
	return 0
}

func (h *Host) contextWindow(modelID string) int64 {
	if v, ok := h.Settings.Get("agent.context_window"); ok {
		switch n := v.(type) {
		case int:
			if n > 0 {
				return int64(n)
			}
		case int64:
			if n > 0 {
				return n
			}
		case float64:
			if n > 0 {
				return int64(n)
			}
		}
	}
	if modelID != "" {
		prov, model, ok := strings.Cut(modelID, "/")
		if ok {
			if spec, ok := h.Providers.Lookup(prov); ok {
				for _, m := range spec.Models {
					if m.ID == model && m.ContextWindow > 0 {
						return m.ContextWindow
					}
				}
			}
		}
	}
	return 200_000
}

func (h *Host) modelExists(id string) bool {
	prov, _, ok := strings.Cut(id, "/")
	if !ok {
		return false
	}
	spec, ok := h.Providers.Lookup(prov)
	if !ok {
		return false
	}
	for _, m := range spec.Models {
		if m.ID == strings.TrimPrefix(id, prov+"/") {
			return true
		}
	}
	return false
}

// pickModel opens the Go-owned model picker overlay (DESIGN §8).
func (h *Host) pickModel() (string, bool) {
	ui := h.hooks()
	if ui == nil {
		return "", false
	}
	var items []ModelOption
	for _, s := range h.Providers.List() {
		for _, m := range s.Models {
			items = append(items, ModelOption{
				ID: s.ID + "/" + m.ID, Display: m.Display, HasKey: h.Providers.HasKey(s.ID),
			})
		}
	}
	cur := ""
	if v, ok := h.Settings.Get("agent.model"); ok {
		if s, ok := v.(string); ok {
			cur = s
		}
	}
	return ui.PickModel(items, cur)
}

func (h *Host) agentNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("profile", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		spec, ok := L.Get(2).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.agent.profile: spec must be a table")
			return 0
		}
		if !userOrProject(h.RT.CurrentOrigin()) {
			L.RaiseError("zemi.agent.profile: only user or project config may define profiles")
			return 0
		}
		p := &Profile{Name: name, Model: rawString(spec, "model"), System: rawString(spec, "system")}
		if temp, ok := spec.RawGetString("temperature").(glua.LNumber); ok {
			t := float64(temp)
			p.Temperature = &t
		}
		if list, ok := spec.RawGetString("tools").(*glua.LTable); ok {
			list.ForEach(func(_, v glua.LValue) {
				if s, ok := v.(glua.LString); ok {
					p.Tools = append(p.Tools, string(s))
				}
			})
		}
		if list, ok := spec.RawGetString("context").(*glua.LTable); ok {
			if files, ok := list.RawGetString("files").(*glua.LTable); ok {
				files.ForEach(func(_, v glua.LValue) {
					if s, ok := v.(glua.LString); ok {
						p.ContextFiles = append(p.ContextFiles, string(s))
					}
				})
			}
		}
		h.agents.mu.Lock()
		h.agents.profiles[name] = p
		h.agents.mu.Unlock()
		return 0
	}))

	tb.RawSetString("set_default", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		if !userOrProject(h.RT.CurrentOrigin()) {
			L.RaiseError("zemi.agent.set_default: only user or project config may set the default profile")
			return 0
		}
		h.agents.mu.Lock()
		_, ok := h.agents.profiles[name]
		if ok {
			h.agents.defProf = name
		}
		h.agents.mu.Unlock()
		if !ok {
			L.RaiseError("zemi.agent.set_default: unknown profile %q", name)
		}
		return 0
	}))

	tb.RawSetString("current", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		ag := h.agents
		ag.mu.Lock()
		out.RawSetString("profile", glua.LString(ag.defProf))
		sid := ag.sessionID
		ag.mu.Unlock()
		out.RawSetString("id", glua.LString(sid))
		out.RawSetString("model", glua.LString(h.currentModelID(nil)))
		L.Push(out)
		return 1
	}))

	// send returns at once: starting a turn builds the system prompt, which
	// runs agent.assemble handlers on this interpreter and must not wait on it.
	tb.RawSetString("send", L.NewFunction(func(L *glua.LState) int {
		text := L.CheckString(1)
		h.GoSafe("zemi.agent.send", func() {
			if err := h.AgentSend(text); err != nil {
				h.Log.Warn("agent", "zemi.agent.send failed", map[string]any{"error": err.Error()})
				h.Notify("error", "agent", err.Error())
			}
		})
		return 0
	}))

	tb.RawSetString("cancel", L.NewFunction(func(L *glua.LState) int {
		h.AgentCancel()
		return 0
	}))

	tb.RawSetString("usage", L.NewFunction(func(L *glua.LState) int {
		s := h.AgentStream()
		out := L.NewTable()
		out.RawSetString("in", glua.LNumber(s.SessionIn))
		out.RawSetString("out", glua.LNumber(s.SessionOut))
		out.RawSetString("total", glua.LNumber(s.SessionIn+s.SessionOut))
		out.RawSetString("context_tokens", glua.LNumber(s.LastIn))
		out.RawSetString("context_window", glua.LNumber(s.CtxWindow))
		pct := 0.0
		if s.CtxWindow > 0 {
			pct = float64(s.LastIn) / float64(s.CtxWindow) * 100
		}
		out.RawSetString("context_pct", glua.LNumber(pct))
		L.Push(out)
		return 1
	}))

	h.agentRunFns(L, tb)
	return tb
}

func userOrProject(o lua.Origin) bool {
	return o == lua.OriginUser || o == lua.OriginProject
}

func (h *Host) permissionNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("rule", L.NewFunction(func(L *glua.LState) int {
		spec, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.permission.rule: rule must be a table")
			return 0
		}
		r := perm.Rule{
			Subject:  rawString(spec, "subject"),
			Pattern:  rawString(spec, "pattern"),
			Decision: perm.Decision(rawString(spec, "decision")),
			NotMatch: rawString(spec, "not_match"),
			Scope:    perm.Scope(rawString(spec, "scope")),
		}
		if r.Subject == "" || r.Pattern == "" {
			L.RaiseError("zemi.permission.rule: subject and pattern are required")
			return 0
		}
		switch r.Decision {
		case perm.Allow, perm.Ask, perm.Deny:
		default:
			L.RaiseError("zemi.permission.rule: decision must be allow, ask or deny")
			return 0
		}
		if r.Scope == "" {
			r.Scope = perm.ScopeSession
		}
		if r.Scope != perm.ScopeSession && r.Scope != perm.ScopePermanent {
			L.RaiseError("zemi.permission.rule: scope must be session or permanent")
			return 0
		}
		origin := h.RT.CurrentOrigin()
		// Origins (DESIGN §5, §6.1): allow rules widen, so they come from
		// user config only; permanent scope survives restarts, so it is
		// user-only too (learned proposals reach it only through the
		// review commit); every other origin may only add session-scope
		// deny/ask rules.
		if origin != lua.OriginUser {
			if r.Decision == perm.Allow {
				L.RaiseError("zemi.permission.rule: allow rules may only come from user config")
				return 0
			}
			if r.Scope == perm.ScopePermanent {
				L.RaiseError("zemi.permission.rule: permanent rules may only come from user config")
				return 0
			}
		}
		if err := validateRuleExamples(r, rawString(spec, "match"), rawString(spec, "not_match_example")); err != nil {
			L.RaiseError("zemi.permission.rule: %v", err)
			return 0
		}
		h.Perm.Add(r)
		if r.Scope == perm.ScopePermanent {
			if err := h.permRegenerate(); err != nil {
				L.RaiseError("zemi.permission.rule: %v", err)
				return 0
			}
			// The keypress is the review (§6.1): a permanent rule lands
			// as a one-line commit; a failed commit is logged, the rule
			// and the dump stay (the revisions table records failure).
			changes, _ := json.Marshal(r)
			title := "rules: " + string(r.Decision) + " " + r.Subject + " " + r.Pattern
			if err := h.commitConfigRevision(title, string(changes),
				[]string{"permissions.lua"}, "user"); err != nil {
				h.Log.Warn("config-repo", "permanent-rule commit failed", map[string]any{
					"error": err.Error(),
				})
			}
		}
		return 0
	}))

	tb.RawSetString("rules", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		for _, r := range h.Perm.Rules() {
			row := L.NewTable()
			row.RawSetString("subject", glua.LString(r.Subject))
			row.RawSetString("pattern", glua.LString(r.Pattern))
			row.RawSetString("decision", glua.LString(r.Decision))
			row.RawSetString("not_match", glua.LString(r.NotMatch))
			row.RawSetString("scope", glua.LString(r.Scope))
			out.Append(row)
		}
		L.Push(out)
		return 1
	}))

	return tb
}

// validateRuleExamples checks Lua-authored rule examples at load (DESIGN
// §9): the match example must match the pattern without being excluded,
// the not_match example must be excluded; a guard needs both examples.
func validateRuleExamples(r perm.Rule, matchEx, notMatchEx string) error {
	if r.NotMatch == "" {
		if matchEx == "" {
			return fmt.Errorf("rule %s %q needs a match example", r.Subject, r.Pattern)
		}
		if !perm.MatchExample(r.Pattern, matchEx) {
			return fmt.Errorf("match example %q does not match pattern %q", matchEx, r.Pattern)
		}
		return nil
	}
	if matchEx == "" || notMatchEx == "" {
		return fmt.Errorf("rule %s %q with not_match needs both examples", r.Subject, r.Pattern)
	}
	if !perm.MatchExample(r.Pattern, matchEx) || perm.MatchExample(r.NotMatch, matchEx) {
		return fmt.Errorf("match example %q must match %q and not be excluded by %q", matchEx, r.Pattern, r.NotMatch)
	}
	if perm.MatchExample(r.Pattern, notMatchEx) && !perm.MatchExample(r.NotMatch, notMatchEx) {
		return fmt.Errorf("not_match example %q must be excluded by %q", notMatchEx, r.NotMatch)
	}
	return nil
}

func (h *Host) sessionNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()
	tb.RawSetString("id", L.NewFunction(func(L *glua.LState) int {
		L.Push(glua.LString(h.AgentSessionID()))
		return 1
	}))
	tb.RawSetString("title", L.NewFunction(func(L *glua.LState) int {
		L.Push(glua.LString(h.Session().Title))
		return 1
	}))
	tb.RawSetString("set_title", L.NewFunction(func(L *glua.LState) int {
		title := L.CheckString(1)
		h.tMu.Lock()
		h.session.Title = title
		h.tMu.Unlock()
		if sid := h.AgentSessionID(); sid != "" && h.DB != nil {
			_ = h.DB.RenameSession(sid, title)
		}
		h.refresh()
		return 0
	}))
	tb.RawSetString("messages", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		sid := h.AgentSessionID()
		if sid != "" && h.DB != nil {
			msgs, _ := h.DB.Messages(sid)
			for _, m := range msgs {
				row := L.NewTable()
				row.RawSetString("id", glua.LString(m.ID))
				row.RawSetString("role", glua.LString(m.Role))
				row.RawSetString("json", glua.LString(m.PartsJSON))
				out.Append(row)
			}
		}
		L.Push(out)
		return 1
	}))
	h.sessionExtraFns(L, tb)
	return tb
}

// ---------------------------------------------------------------------------
// permission gate for model tool calls

// askHook is the tools.Pipeline asker: veto event, then the permission
// mode (yolo allows, auto lets a model approve routine requests), then the
// Go-owned overlay and the "always" rule storage (DESIGN §6.2, §8).
func (h *Host) askHook() tools.Asker {
	return func(req tools.Request) (bool, string) {
		resolved := func(allowed bool, by string) (bool, string) {
			h.EmitAsync("permission.resolved", map[string]any{
				"subject": req.Subject, "arg": req.Arg,
				"decision": map[bool]string{true: "allow", false: "deny"}[allowed],
				"by":       by,
			})
			return allowed, by
		}
		res := h.EmitVetoable(context.Background(), "permission.request",
			map[string]any{"subject": req.Subject, "arg": req.Arg, "reason": req.Reason},
			lua.VetoDeadline)
		if res.Denied {
			return resolved(false, "event")
		}
		switch h.PermMode() {
		case "yolo":
			return resolved(true, "mode:yolo")
		case "auto":
			ok, why := h.autoApprove(req)
			if ok {
				return resolved(true, "mode:auto")
			}
			h.Log.Info("permissions", "auto mode deferred to the user", map[string]any{"subject": req.Subject, "why": why})
			req.Reason = why
		}
		ui := h.hooks()
		if ui == nil {
			return resolved(false, "default")
		}
		allowed, by := ui.AskPermission(PermRequest{Subject: req.Subject, Arg: req.Arg, Reason: req.Reason, Preview: req.Preview})
		if allowed && by == "user:always" {
			h.storeAlwaysRules(req)
		}
		return resolved(allowed, by)
	}
}

// storeAlwaysRules records an "always" answer as session rules (DESIGN
// §6.2): for bash, one exact rule per normalized segment; otherwise the
// exact argument. Scoped to the model principal.
func (h *Host) storeAlwaysRules(req tools.Request) {
	const principal = "model"
	if req.Subject == "tool.bash" {
		n := perm.Normalize(req.Arg)
		if n.Fail != "" || n.ForceAsk != "" {
			return // gate-forced asks always re-ask; no rule satisfies them
		}
		for _, seg := range n.Segments {
			h.Perm.Add(perm.Rule{Subject: "tool.bash", Pattern: seg, Decision: perm.Allow, Principal: principal})
		}
		return
	}
	h.Perm.Add(perm.Rule{Subject: req.Subject, Pattern: req.Arg, Decision: perm.Allow, Principal: principal})
}

// toolPostHook emits tool.post after every builtin tool run.
func (h *Host) toolPostHook() func(string, map[string]any, bool, string) {
	return func(name string, input map[string]any, ok bool, output string) {
		h.EmitAsync("tool.post", map[string]any{
			"tool": name, "input": input, "ok": ok,
			"output": truncateString(output, 4096),
		})
	}
}

func truncateString(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// PipelineFor builds the tools pipeline for the model's tool calls.
func (h *Host) PipelineFor() *tools.Pipeline {
	p := &tools.Pipeline{
		Store:     h.Perm,
		Pre:        h.toolPreHook(),
		Ask:        h.askHook(),
		Checkpoint: h.checkpointFile,
		Plan:       func() bool { return h.PermMode() == "plan" },
		Denied: func(tool string, req tools.Request, v tools.Verdict) {
			if len(h.Events.Handlers("tool.denied")) > 0 {
				h.EmitAsync("tool.denied", map[string]any{"tool": tool, "subject": req.Subject, "arg": req.Arg, "by": v.DecidedBy, "reason": v.Reason})
			}
		},
		Post:      h.toolPostHook(),
		Principal: "model",
		Jobs:      h.jobs,
		Skills:    h.skillIndex,
		Remember: func(scope, kind, text string, tags []string) (bool, error) {
			stored, _, err := h.proposeMemory(scope, kind, text, tags)
			return stored, err
		},
		Recall: func(query string, limit int) ([]*store.Memory, error) {
			return h.memorySearch("all", query, limit)
		},
		Question: func(q tools.Question) (string, bool) {
			ui := h.hooks()
			if ui == nil {
				return "", false
			}
			return ui.AskQuestion(q)
		},
	}
	if h.DB != nil {
		p.TodosRead = func() ([]store.Todo, error) { return h.DB.Todos(h.AgentSessionID()) }
		p.TodosWrite = func(todos []store.Todo) error {
			sid := h.AgentSessionID()
			if sid == "" {
				return fmt.Errorf("no active session")
			}
			defer h.todosStale.Store(true)
			return h.DB.SetTodos(sid, todos)
		}
	}
	return p
}
