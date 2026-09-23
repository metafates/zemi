// agent.go is the agent core in the host (DESIGN §6.3): the
// provider/model/agent/permission/session namespaces, profile resolution,
// turn streaming into the UI, message persistence and the steering queue.
package hostapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sandbox"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
	glua "github.com/yuin/gopher-lua"
)

// Profile is one named agent shape: model, system prompt, tools.
// ReadOnly makes it a planning agent: the project is read-only to it and
// the pipeline denies file changes and forces asks (see h.readOnly).
type Profile struct {
	Name         string   `json:"name"`
	Model        string   `json:"model"`
	System       string   `json:"system"`
	Tools        []string `json:"tools"`
	ContextFiles []string `json:"context_files"`
	ReadOnly     bool     `json:"read_only"`
	Temperature  *float64 `json:"temperature,omitempty"`
}

// PermRequest is one interactive permission ask for the overlay.
type PermRequest struct {
	Tool   string // the asking tool (bash, edit, mcp_github_search)
	Always string // what the session-long choice allows ("allow `go test`"), "" when none is offered
	// Wider is a second session-long choice covering more than Always (a
	// read's whole repository), "" when there is none
	Wider string
	// Project is a persistent choice covering Always's scope but written to
	// the project's permissions file (a trusted bash command), "" when none
	Project string
	Subject string
	Arg     string
	Reason  string
	Preview string // file tools: the proposed change as "- "/"+ " lines
	// Description is the model's summary of a command, not checked against it
	Description string
	// Outside means this bash command escapes the sandbox and can reach the
	// whole machine; the prompt shows a warning band for it
	Outside bool
}

// StreamTool is one tool call in the live stream view (DESIGN §6.3).
type StreamTool struct {
	ID      string
	Label   string // the model's short description of a command
	Started time.Time
	Detail  string // live progress line (subagents, workflows, a bash command's last output line)
	Run     string // agent tool: the run id, so the row can open its transcript
	Diff    string // file tools: the change as "- "/"+ " lines
	Output  string // the last lines of the result (shortOutput)
	Name    string
	Input   string
	Status  string // "run", "ok", "err"
	partial string // the head of the streaming input, until Label is known
}

// agentHost owns the live agent session: profiles, the runner, the
// steering queue and the stream view the UI renders.
type agentHost struct {
	mu        sync.Mutex
	profiles  map[string]*Profile
	runner    *agent.Runner
	sessionID string

	active    bool
	textBuf   string
	reasonBuf string
	// stepText and stepReason are the buffer lengths when the current step
	// began; a provider retry streams the step again from there.
	stepText   int
	stepReason int
	tools      []StreamTool
	steering   []string
	modelSeed  string
	turnModel  string // model id the running turn was built with, for its price

	// session usage (DESIGN §8 statusline tally): sessIn/sessOut accumulate
	// every turn's counts, lastIn is the context the latest step used
	// (agent.ContextUsed, the current context estimate). ctxWin: seeded
	// override (tests); 0 resolves via contextWindow.
	sessIn  int64
	sessOut int64
	// cache reads and the input billed with them, for the /usage share
	cacheRead   int64
	cacheBilled int64
	lastIn      int64
	ctxWin      int64
	sessCost    float64 // USD, from model spec prices

	// readMu guards reads and instr, the per-session file bookkeeping the
	// tools pipeline carries (what the model has read, which instruction
	// files it has been given). Both are dropped when the session changes.
	readMu sync.Mutex
	reads  map[string]*sync.Map
	instr  map[string]bool

	// promptTokens estimates the system prompt and tool schemas the runner
	// carries; every prompt holds them, so the context estimate counts them.
	// promptParts is the same total broken down for /usage, promptText the
	// system prompt itself for /prompt.
	promptTokens int64
	promptParts  []promptPart
	promptText   string

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

	// compactFails counts auto compactions in a row that failed or left the
	// context full; at compactMaxFails auto compaction stops for the session.
	// compacted has the next step judge the last compaction. holdOverflow
	// keeps TurnDone quiet about a context-too-long error sendCompacting
	// will retry.
	compactFails int
	compacted    bool
	holdOverflow bool
	holdImages   bool          // a 400 over image parts is resent without them, not reported
	holdCap      bool          // a refused output cap is resent without it, not reported
	holdCut      bool          // a Full cut is compacted and resent, not reported; off once the compaction failed
	cutUsage     fantasy.Usage // the steps before a Full cut, billed with the turn's end

	// digest is pinned at session start (DESIGN §10.7) and refreshed only
	// at compaction; memAuto counts auto-applied memories against the
	// session cap (memory proposals persist in the store).
	digest     string
	memAuto    int
	reflectErr error // last reflector run's error, nil after a success (/health)

	// turnMsgID is the stored user message of the running turn; write tools
	// checkpoint files against it (rewind).
	turnMsgID string
	// shellNotes holds !command output waiting for the next message.
	shellNotes []string

	// todoNag nudges a stale in_progress todo: the item it was set for,
	// how many tool calls have run since, and whether a remind is already
	// queued. Any todos write resets it, so an update (or a tick) stops it.
	todoInProgress string
	todoCalls      int
	todoPending    bool

	// planApproved is set when a read-only agent's plan is approved; the ended
	// turn's planFollowup starts implementation as the restored agent. Cleared
	// by takePlanApproved.
	planApproved string
	// planNudged: the planner was told once this user message that it ended
	// in plain text; it is not told again until the user speaks
	planNudged bool

	// titled is false for a new session until its first exchange named it
	// (autoTitle); firstUser is that session's first message.
	titled    bool
	firstUser string
	env       string // pinned environment section (environmentPrompt)
}

func newAgentHost() *agentHost {
	return &agentHost{profiles: map[string]*Profile{}}
}

// ---------------------------------------------------------------------------
// host wiring

// Effort is the configured reasoning effort ("" = provider default).
func (h *Host) Effort() string { return h.Settings.Str("agent.effort") }

// resolveProfile returns the profile to run: explicit name, else the
// agent.profile key. default is the implicit shipped shape (nil); planner
// is the shipped read-only agent. Any other name must be defined.
func (h *Host) resolveProfile(name string) (*Profile, error) {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if name == "" {
		name = h.Settings.Str("agent.profile")
	}
	if name == "planner" {
		return &Profile{Name: "planner", ReadOnly: true}, nil
	}
	if p, ok := ag.profiles[name]; ok {
		return p, nil
	}
	if name != "default" {
		return nil, fmt.Errorf("agent: unknown profile %q", name)
	}
	return nil, nil
}

// readOnly reports whether the active agent is read-only: the project is
// read-only to it and file changes are denied regardless of the mode.
func (h *Host) readOnly() bool {
	p, _ := h.resolveProfile("")
	return p != nil && p.ReadOnly
}

// currentModelID picks the model the main conversation runs on: a profile
// override, else the smart tier.
func (h *Host) currentModelID(p *Profile) string {
	if p != nil && p.Model != "" {
		return h.modelRef(p.Model)
	}
	return h.tierModel("smart")
}

// liveModelID is the model turns run on: the running turn's, else the
// active profile's (ag.mu held).
func (h *Host) liveModelID() string {
	ag := h.agents
	if ag.active && ag.turnModel != "" {
		return ag.turnModel
	}
	return h.currentModelID(ag.profiles[h.Settings.Str("agent.profile")])
}

// sessionModelID is liveModelID for callers not holding ag.mu.
func (h *Host) sessionModelID() string {
	h.agents.mu.Lock()
	defer h.agents.mu.Unlock()
	return h.liveModelID()
}

func (h *Host) builtinTools(pipe *tools.Pipeline) []fantasy.AgentTool {
	return append(tools.All(pipe), h.agentTool(), h.workflowTool(), h.luaEvalTool())
}

// headlessExcluded needs a user at a terminal: with no UI hooks these tools
// can only refuse, so a print run does not pay for their schemas every step.
var headlessExcluded = map[string]bool{tools.ToolQuestion: true, tools.ToolPlan: true}

// toolSkip adds the language server tools to skip when no server can start
// in this project (untrusted, or none installed for it): both could then
// only answer with why they cannot, and their schemas ride every step.
func (h *Host) toolSkip(base map[string]bool) map[string]bool {
	if len(h.lspDefs("")) > 0 {
		return base
	}
	skip := map[string]bool{tools.ToolLSP: true, tools.ToolDiagnostics: true}
	maps.Copy(skip, base)
	return skip
}

// profileTools selects the tool set: a profile's names filter the builtin,
// Lua and MCP tools alike, and a name nothing answers to is recorded rather
// than dropped, so one misspelling does not leave the agent toolless.
func (h *Host) profileTools(p *Profile) []fantasy.AgentTool {
	var want []string
	if p != nil {
		want = p.Tools
	}
	var skip map[string]bool
	if h.hooks() == nil {
		skip = headlessExcluded
	}
	skip = h.toolSkip(skip)
	pipe := h.PipelineFor()
	all := append(append(h.builtinTools(pipe), h.luaFantasyTools(pipe)...), h.mcpFantasyTools(pipe)...)
	out, unknown := filterTools(all, want, skip)
	if len(unknown) > 0 {
		h.RecordError("profile "+p.Name, "no tool is named "+strings.Join(unknown, ", "))
	}
	return out
}

func (h *Host) ensureRunner(p *Profile) (*agent.Runner, error) {
	model, err := h.resolveModel(p)
	if err != nil {
		return nil, err
	}
	toolset := h.profileTools(p)
	sys, parts := h.buildSystemParts(p, toolset)
	r := agent.NewRunner(model, toolset, sys)
	sizes := promptSizes(parts, toolset)
	chars := 0
	for _, s := range sizes {
		chars += s.chars
	}
	h.agents.mu.Lock()
	h.agents.promptParts, h.agents.promptTokens, h.agents.promptText = sizes, int64(chars/4), sys
	h.agents.mu.Unlock()
	r.ProviderOptions = h.Providers.CallOptions(h.currentModelID(p), h.Effort())
	r.MaxOutputTokens = h.maxOutput(h.currentModelID(p))
	if p != nil {
		r.Temperature = p.Temperature
	}
	r.Hooks = h.streamHooks()
	r.Persist = h.persistMessage
	r.Steer = h.deliverSteering
	r.TextOnly = !h.takesImages(h.currentModelID(p))
	r.Compose = func(text string) fantasy.Message { return h.composeUserMessage(text, !r.TextOnly) }
	r.Full = h.contextFull
	return r, nil
}

// promptPart is one named section of the fixed prompt, in characters.
type promptPart struct {
	name  string
	chars int
}

// promptSizes breaks down what every prompt carries besides the messages:
// the system prompt's sections plus the tool schemas sent with it, MCP
// apart since a server's whole catalog rides every request whether the
// model calls it or not.
func promptSizes(p promptParts, toolset []fantasy.AgentTool) []promptPart {
	builtin, mcp := 0, 0
	for _, t := range toolset {
		info := t.Info()
		schema, _ := json.Marshal(info.Parameters)
		n := len(info.Name) + len(info.Description) + len(schema)
		if strings.HasPrefix(info.Name, mcpToolPrefix) {
			mcp += n
		} else {
			builtin += n
		}
	}
	return []promptPart{
		{"system", p.base}, {"tools", builtin}, {"mcp", mcp},
		{"instructions", p.instructions}, {"lua", p.lua}, {"skills", p.skills}, {"memory", p.digest},
	}
}

// SystemPrompt is the system prompt the last send assembled, "" before the
// first one. /usage measures it; this is the text, so a profile, an
// assemble handler or an instruction file the budget cut can be read
// instead of guessed at. It is never rebuilt here: assembling it runs Lua
// handlers on the interpreter, which a Lua caller would deadlock on.
func (h *Host) SystemPrompt() string {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return ag.promptText
}

// promptBreakdown is the fixed prompt the last runner build measured, in
// tokens: what every request pays for before the first message.
func (h *Host) promptBreakdown() ([]promptPart, int64) {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return ag.promptParts, ag.promptTokens
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
	// Held from here on: building the prompt waits on the interpreter, and a
	// send arriving meanwhile must queue instead of starting a second turn.
	ag.active = true
	scope := h.beginScope()
	ag.mu.Unlock()
	return h.sendClaimed(scope, text)
}

// drainQueue starts a turn for what a background agent's report left queued
// (it arrived under a claim that ran no turn, the plan popup's) once none is
// left and no turn holds the agent. Runs on the run's goroutine: the send
// waits on the interpreter.
func (h *Host) drainQueue() {
	ag := h.agents
	ag.mu.Lock()
	if ag.active || len(ag.steering) == 0 || h.tasks.runningFor(ag.sessionID) > 0 {
		ag.mu.Unlock()
		return
	}
	text := joinQueue(ag.steering)
	ag.steering = nil
	ag.active = true
	scope := h.beginScope()
	ag.mu.Unlock()
	if err := h.sendClaimed(scope, text); err != nil {
		h.Notify("error", "Could not send", err.Error())
	}
}

// sendClaimed starts a turn for text on an agent already claimed with scope.
func (h *Host) sendClaimed(scope *turnScope, text string) error {
	ag := h.agents
	fail := func(err error) error {
		h.endScope(scope)
		h.refresh()
		return err
	}
	p, err := h.resolveProfile("")
	if err != nil {
		return fail(err)
	}
	// before the session row, so a send that cannot run leaves no empty session
	if _, err := h.resolveModel(p); err != nil {
		return fail(err)
	}
	if err := h.ensureSession(p, text); err != nil {
		return fail(err)
	}
	runner, err := h.ensureRunner(p)
	if err != nil {
		return fail(err)
	}

	ag.mu.Lock()
	// The runner is rebuilt per send (model, prompt and tools may have
	// changed); the conversation carries over from the idle previous one.
	if prev := ag.runner; prev != nil {
		runner.History = prev.History
	}
	ag.runner = runner
	ag.modelSeed = ""
	ag.turnModel = h.currentModelID(p)
	ag.beginTurn()
	ag.mu.Unlock()
	h.startTurn(text)

	h.GoSafe("agent turn", func() {
		h.runTurns(runner, scope, text)
		h.planFollowup(runner)
	})
	return nil
}

// beginScope starts a turn scope (ag.mu held). It is stored when the agent
// is claimed, so a cancel arriving before the turn goroutine runs is kept.
func (h *Host) beginScope() *turnScope {
	ctx, cancel := context.WithCancel(context.Background())
	t := &turnScope{ctx: ctx, cancel: cancel}
	h.turn.Store(t)
	return t
}

// endScope releases the agent claimed with scope.
func (h *Host) endScope(scope *turnScope) {
	ag := h.agents
	ag.mu.Lock()
	ag.active = false
	h.turn.Store(nil)
	ag.mu.Unlock()
	scope.cancel()
}

// handOff ends a claim that ran no turn (/compact, rewind). Messages queued
// meanwhile start under the same claim, so a submit cannot jump ahead of
// them; after a cancel the queue waits for the UI to hand it back.
func (h *Host) handOff(scope *turnScope) {
	ag := h.agents
	ag.mu.Lock()
	queued := ""
	if scope.ctx.Err() == nil {
		queued = joinQueue(ag.steering)
		ag.steering = nil
	}
	if queued == "" {
		ag.active = false
		h.turn.Store(nil)
	}
	ag.mu.Unlock()
	if queued == "" {
		scope.cancel()
		h.refresh()
		return
	}
	// sending waits on the interpreter, and rewind may be called from Lua
	h.GoSafe("agent send", func() {
		if err := h.sendClaimed(scope, queued); err != nil {
			h.Notify("error", "Could not send", err.Error())
		}
	})
}

// runTurns sends text and every follow-up the queue produces on one
// runner, compacting first whenever the last prompt filled the context.
func (h *Host) runTurns(runner *agent.Runner, scope *turnScope, text string) {
	defer h.endTurnAfterPanic()
	for scope != nil {
		// one context per turn covers compaction too, which runs before the
		// runner holds a cancel of its own
		if h.compactDue() {
			h.compactReporting(scope.ctx, runner, compactAuto, "")
		}
		h.sendCompacting(scope.ctx, runner, text)
		scope, text = h.nextTurn(scope)
	}
}

func (h *Host) startTurn(text string) {
	// what follows attachMarker is for the model (background reports), the
	// same cut resumed transcripts make
	if shown, _, _ := strings.Cut(text, attachMarker); shown != "" {
		h.AppendMessage("user", shown)
		h.agents.mu.Lock()
		h.agents.planNudged = false
		h.agents.mu.Unlock()
	}
	h.agentStream()
	h.EmitAsync("agent.start", map[string]any{"text": text})
}

// nextTurn runs after each turn: messages queued after the last step
// boundary become one follow-up turn, otherwise the agent goes idle.
// active stays held across the handoff, so a submit in between queues
// instead of starting a turn out of order. An approved plan (planApproved)
// is a follow-up too, kept under the same claim, but sent afresh: the
// restored profile needs its own runner, not the planner's. After a
// cancelled turn nothing starts: the queue waits for the UI to hand it back
// to the composer.
func (h *Host) nextTurn(done *turnScope) (*turnScope, string) {
	ag := h.agents
	ag.mu.Lock()
	text, handoff := "", false
	if done.ctx.Err() == nil {
		if ag.planApproved != "" {
			ag.steering = append(ag.steering, ag.planApproved)
			ag.planApproved, handoff = "", true
		}
		text = joinQueue(ag.steering)
		ag.steering = nil
	}
	var next *turnScope
	switch {
	case text == "":
		ag.active = false
		h.turn.Store(nil)
	case handoff:
		next = h.beginScope() // sendClaimed begins the turn
	default:
		ag.beginTurn()
		next = h.beginScope()
	}
	ag.mu.Unlock()
	done.cancel()
	switch {
	case text == "":
		h.refresh()
	case handoff:
		if err := h.sendClaimed(next, text); err != nil {
			h.Notify("error", "Could not start the implementation", err.Error())
		}
		return nil, ""
	default:
		h.startTurn(text)
	}
	return next, text
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
	shown := make([]string, len(msgs))
	for i, m := range msgs {
		shown[i], _, _ = strings.Cut(m, attachMarker)
	}
	if len(msgs) > 0 {
		ag.steering, ag.textBuf, ag.reasonBuf, ag.tools, ag.thought = nil, "", "", nil, 0
	}
	ag.mu.Unlock()
	if len(msgs) == 0 {
		return nil
	}
	h.appendTurnOutput(reason, stat, tools, text, TurnStat{})
	for _, s := range shown {
		if s == "" {
			continue // a model-only message (attachMarker, no shown part)
		}
		h.AppendMessage("user", s)
	}
	return msgs // Runner.Compose expands them
}

// AgentTakeQueue removes and returns the messages still waiting for a step
// boundary, so the UI can hand them back to the composer.
func (h *Host) AgentTakeQueue() []string {
	ag := h.agents
	ag.mu.Lock()
	var q, reports []string
	for _, m := range ag.steering {
		if strings.Contains(m, attachMarker) {
			// a background report is for the model, not the composer: it
			// waits for the next turn
			reports = append(reports, m)
		} else {
			q = append(q, m)
		}
	}
	ag.steering = reports
	ag.mu.Unlock()
	if len(q) > 0 {
		h.refresh()
	}
	return q
}

// joinQueue makes queued messages one message with every shown part ahead
// of the first attachMarker, where each reader cuts the text.
func joinQueue(msgs []string) string {
	shown := make([]string, len(msgs))
	tails := ""
	for i, m := range msgs {
		s, tail, ok := strings.Cut(m, attachMarker)
		if ok {
			tails += attachMarker + tail
		}
		shown[i] = s
	}
	return strings.Join(shown, "\n\n") + tails
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
	if t := h.turn.Load(); t != nil {
		t.cancel()
	}
}

func (h *Host) AgentActive() bool {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return ag.active
}

func (h *Host) agentStream() {
	if ui := h.hooks(); ui != nil {
		ui.AgentStream()
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
	s.h.agentStream()
	// NOTE: one queue round trip per delta; fine for the few configs that
	// listen, coalesce here if one ever streams into something slow.
	s.h.EmitAsync("agent.text_delta", map[string]any{"text": text})
}

func (s *streamAdapter) StepStart() {
	ag := s.h.agents
	ag.mu.Lock()
	ag.stepText, ag.stepReason = len(ag.textBuf), len(ag.reasonBuf)
	ag.mu.Unlock()
}

func (s *streamAdapter) Retrying(err error, delay time.Duration) {
	ag := s.h.agents
	ag.mu.Lock()
	ag.textBuf = ag.textBuf[:min(ag.stepText, len(ag.textBuf))]
	ag.reasonBuf = ag.reasonBuf[:min(ag.stepReason, len(ag.reasonBuf))]
	ag.retries++
	ag.retryAt = time.Now().Add(delay)
	ag.retryWhy, _ = s.h.explain(err) // the advice is to wait, which the countdown says
	why := ag.retryWhy
	ag.mu.Unlock()
	s.h.Log.Warn("agent", "provider retry", map[string]any{"error": errString(err), "delay_ms": delay.Milliseconds()})
	if s.h.hooks() == nil { // print mode: the countdown is on no screen
		fmt.Fprintf(os.Stderr, "zemi: retrying in %s: %s\n", delay.Round(time.Second), why)
	}
	s.h.agentStream()
}

func (s *streamAdapter) ReasoningDelta(text string) {
	ag := s.h.agents
	ag.mu.Lock()
	ag.beginThinking()
	ag.reasonBuf += text
	ag.mu.Unlock()
	s.h.agentStream()
	s.h.EmitAsync("agent.reasoning_delta", map[string]any{"text": text})
}

// ToolCallPending shows the call while the model is still streaming its
// arguments: a write's file body or a long heredoc takes seconds that a
// spinner alone says nothing about.
func (s *streamAdapter) ToolCallPending(id, name string) {
	s.openToolRow(StreamTool{ID: id, Name: name, Started: time.Now(), Status: "run"})
}

func (s *streamAdapter) ToolCallStart(id, name, input string) {
	// the diff reads the file: outside the lock, which UI readers share
	diff := ""
	if name == tools.ToolWrite || name == tools.ToolEdit || name == tools.ToolMultiedit {
		var in map[string]any
		if json.Unmarshal([]byte(input), &in) == nil {
			diff = capLines(tools.ChangeDiff(name, in), 24)
		}
	}
	row := StreamTool{ID: id, Name: name, Input: toolSummary(input), Label: toolLabel(input), Started: time.Now(), Status: "run", Diff: diff}
	ag := s.h.agents
	ag.mu.Lock()
	i := slices.IndexFunc(ag.tools, func(t StreamTool) bool { return t.ID == id })
	if i < 0 {
		ag.mu.Unlock()
		s.openToolRow(row) // this provider reports no input start
		return
	}
	row.Started = ag.tools[i].Started
	ag.tools[i] = row
	ag.mu.Unlock()
	s.h.agentStream()
}

// openToolRow appends a live tool row. Text streamed before the call
// belongs above it: it and the earlier (finished) tool rows move into the
// transcript, in order, so the next step's text starts its own block
// instead of running on.
func (s *streamAdapter) openToolRow(row StreamTool) {
	ag := s.h.agents
	ag.mu.Lock()
	ag.endThinking()
	reason, tools, text := ag.reasonBuf, ag.tools, ag.textBuf
	stat := TurnStat{ThoughtMS: ag.thought.Milliseconds()}
	flush := strings.TrimSpace(text) != ""
	if flush {
		ag.reasonBuf, ag.tools, ag.textBuf, ag.thought = "", nil, "", 0
	}
	ag.tools = append(ag.tools, row)
	ag.mu.Unlock()
	if flush {
		s.h.appendTurnOutput(reason, stat, tools, text, TurnStat{})
	}
	s.h.agentStream()
}

func (s *streamAdapter) ToolCallEnd(id, name string, ok bool, output, changed string) {
	ag := s.h.agents
	diff := ""
	ag.mu.Lock()
	for i := len(ag.tools) - 1; i >= 0; i-- {
		if ag.tools[i].ID == id && ag.tools[i].Status == "run" {
			ag.tools[i].Status = map[bool]string{true: "ok", false: "err"}[ok]
			ag.tools[i].Output = shortOutput(output)
			if !ok {
				ag.tools[i].Diff = "" // the change did not happen
			}
			if changed != "" {
				ag.tools[i].Diff = changed // bash: what the command changed, failed or not
			}
			diff = ag.tools[i].Diff
			break
		}
	}
	ag.mu.Unlock()
	s.h.agentStream()
	s.h.recordToolCall(s.h.AgentSessionID(), id, name, diff, ok)
	if name != tools.ToolTodos {
		s.h.maybeNagTodos()
	}
}

// StepDone bills a completed step's usage to the live session tally, so the
// token count moves during the turn instead of only at TurnDone.
func (s *streamAdapter) StepDone(usage fantasy.Usage) {
	h := s.h
	ag := h.agents
	ag.mu.Lock()
	sid, model := ag.sessionID, ag.turnModel
	ag.mu.Unlock()
	if model == "" {
		model = h.currentModelID(nil) // a turn the host did not start (tests)
	}
	h.bill(sid, model, usage)
}

func (s *streamAdapter) TurnDone(err error, usage fantasy.Usage) {
	h := s.h
	ag := h.agents
	ag.mu.Lock()
	// sendCompacting resends a held error or reports it; the steps before it
	// still ran, so their usage and rows are kept
	hold := ag.holdOverflow && contextTooLarge(err) || ag.holdImages && imagesRefused(err) || ag.holdCap && maxTokensRefused(err)
	ag.holdOverflow, ag.holdImages, ag.holdCap = false, false, false
	// a Full cut ends the send, not the turn: sendCompacting compacts and
	// sends again, so the finish, the notification and the title wait, and
	// the cut steps' usage joins the final payload
	if err == nil && ag.holdCut && ag.runner != nil && ag.runner.CutShort() {
		ag.cutUsage = addUsage(ag.cutUsage, usage)
		hold = true
	} else {
		usage, ag.cutUsage = addUsage(usage, ag.cutUsage), fantasy.Usage{}
	}
	ag.holdCut = false
	model := ag.turnModel
	ag.mu.Unlock()
	if model == "" {
		model = h.currentModelID(nil) // a turn the host did not start (tests)
	}
	ag.mu.Lock()
	ag.endThinking()
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

	h.appendTurnOutput(reason, TurnStat{ThoughtMS: stat.ThoughtMS, ElapsedMS: stat.ElapsedMS}, tools, text, stat)
	if hold {
		return
	}
	if ui := h.hooks(); ui != nil {
		ui.TurnDone(errString(err))
	}
	if err == nil && strings.TrimSpace(text) != "" {
		ag.mu.Lock()
		first := !ag.titled && ag.sessionID == sid
		ag.titled = true
		user := ag.firstUser
		ag.mu.Unlock()
		if first {
			h.autoTitle(sid, user, text)
		}
	}
	if err != nil {
		h.EmitAsync("agent.error", map[string]any{"error": errString(err)})
		// The failure stays in the transcript: a toast expires, and a turn
		// that ended silently looks like zemi ignored the message.
		row := ChatMessage{Role: "assistant", Kind: "error", Text: truncRunes(sanitize.String(h.errorRowText(err)), 400)}
		if errors.Is(err, context.Canceled) {
			row.Status = "cancelled"
		}
		h.AppendChat(row)
	} else {
		h.EmitAsync("agent.finish", map[string]any{"session": sid, "model": model, "usage": map[string]any{
			"in": billedInput(usage), "out": usage.OutputTokens, "total": billedInput(usage) + usage.OutputTokens,
			"cost": h.turnCost(model, usage),
		}})
	}
}

// billedInput is a step's input tokens with cache reads and writes: the
// session paid for them, and leaving them out would show a token total the
// cost does not match.
func billedInput(u fantasy.Usage) int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
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
			t.Diff = ""
		}
		h.AppendChat(ChatMessage{Role: "assistant", Kind: "tool", Tool: t.Name, Text: t.Input, Label: t.Label, Status: status, Detail: t.Detail, Run: t.Run, Diff: t.Diff, Output: t.Output})
	}
	if strings.TrimSpace(text) != "" {
		h.AppendTurnResult(text, textStat)
	}
}

func addUsage(a, b fantasy.Usage) fantasy.Usage {
	a.InputTokens += b.InputTokens
	a.OutputTokens += b.OutputTokens
	a.TotalTokens += b.TotalTokens
	a.CacheReadTokens += b.CacheReadTokens
	a.CacheCreationTokens += b.CacheCreationTokens
	return a
}

// capLines keeps the first n lines and notes how many were cut.
func capLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n... %d more lines", len(lines)-n)
}

// toolLabel is what a model says a call is about, shown in place of its
// input. Command rows stored before the argument was renamed hold
// "description", which other tools use for something else.
func toolLabel(input string) string {
	var in struct {
		Command     string `json:"command"`
		About       string `json:"about"`
		Description string `json:"description"`
	}
	if json.Unmarshal([]byte(input), &in) != nil {
		return ""
	}
	if in.Command == "" {
		in.Description = ""
	}
	return cleanLabel(cmp.Or(in.About, in.Description))
}

func cleanLabel(s string) string {
	return learn.Truncate(strings.Join(strings.Fields(s), " "), 80)
}

// aboutArg is a call's leading "about" argument in input that is still streaming.
var aboutArg = regexp.MustCompile(`^\s*\{\s*"about"\s*:\s*("(?:[^"\\]|\\.)*")`)

// ToolInputDelta labels a pending row as soon as "about" has streamed: a
// long command or heredoc takes seconds to arrive after it.
func (s *streamAdapter) ToolInputDelta(id, delta string) {
	ag := s.h.agents
	ag.mu.Lock()
	i := slices.IndexFunc(ag.tools, func(t StreamTool) bool { return t.ID == id })
	if i < 0 || ag.tools[i].Label != "" || len(ag.tools[i].partial) > 1024 {
		ag.mu.Unlock()
		return
	}
	t := &ag.tools[i]
	t.partial += delta
	var label string
	if m := aboutArg.FindStringSubmatch(t.partial); m != nil && json.Unmarshal([]byte(m[1]), &label) == nil {
		t.Label = cleanLabel(label)
	}
	labelled := t.Label != ""
	ag.mu.Unlock()
	if labelled {
		s.h.agentStream()
	}
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
	if qs, ok := m["questions"].([]any); ok && len(qs) > 0 {
		q, _ := qs[0].(map[string]any)
		s, _ := q["question"].(string)
		if len(qs) > 1 {
			s += fmt.Sprintf(" +%d more", len(qs)-1)
		}
		return s
	}
	if plan, ok := m["plan"].(string); ok {
		// the plan itself shows as a message above the row: name its first
		// line, less a "Plan" the row's verb already says
		for line := range strings.Lines(plan) {
			if line = strings.TrimSpace(strings.TrimLeft(line, "# \t")); line != "" {
				// match on the original: lowercasing can change the byte length
				if len(line) >= 4 && strings.EqualFold(line[:4], "plan") {
					if rest := line[4:]; rest == "" || rest[0] == ':' || rest[0] == ' ' {
						line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ":"))
					}
				}
				return line
			}
		}
		return ""
	}
	if cmd, ok := m["command"].(string); ok && cmd != "" {
		if args, ok := m["args"].([]any); ok {
			for _, a := range args {
				cmd += " " + fmt.Sprint(a)
			}
		}
		return cmd
	}
	// the lsp tool names what it does in "action"; without it a row would
	// read as the whole body of what it is about to show
	if action, ok := m["action"].(string); ok && action != "" {
		for _, k := range []string{"cmd", "keys", "title", "symbol", "path", "id"} {
			if v, ok := m[k]; ok {
				if s := fmt.Sprint(v); s != "" {
					return action + " " + s
				}
			}
		}
		return action
	}
	// the agent tool's description is optional; without it the row reads
	// the agent and the task instead of the input JSON
	if a, ok := m["agent"].(string); ok && a != "" {
		if d, _ := m["description"].(string); d != "" {
			return d
		}
		prompt, _ := m["prompt"].(string)
		return strings.TrimSuffix(a+": "+truncRunes(strings.Join(strings.Fields(prompt), " "), 80), ": ")
	}
	for _, k := range []string{"path", "file_path", "pattern", "url", "query", "question", "name", "text", "description", "id"} {
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

func (h *Host) persistMessage(role string, msg fantasy.Message) error {
	sid := h.AgentSessionID()
	if h.DB == nil || sid == "" {
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
		h.agents.mu.Lock()
		h.agents.turnMsgID = row.ID
		h.agents.mu.Unlock()
	}
	return nil
}

// recordToolCall stores one tool telemetry row in session sid (each tool
// call gets its own "tool" message row so tool_calls.message_id stays a
// real FK). The output is not kept: the history row already holds it. A
// file tool's diff is, keyed by call id: it is computed against the file
// as it was, so a resumed session could never rebuild it (historyFrom).
func (h *Host) recordToolCall(sid, id, name, diff string, ok bool) {
	if h.DB == nil || sid == "" {
		return
	}
	row := map[string]any{"tool": name}
	if diff != "" {
		row["id"], row["diff"] = id, diff
	}
	parts, _ := json.Marshal(row)
	msg := &store.Message{SessionID: sid, Role: "tool", PartsJSON: string(parts)}
	if err := h.DB.AddToolCall(msg, &store.ToolCall{Tool: name, OK: ok}); err != nil {
		h.Log.Warn("agent", "tool call persist failed", map[string]any{"error": err.Error()})
	}
}

// todoNagAfter is how many of the main agent's tool calls an in_progress
// todo may stay static before zemi nudges the model to tick it (a "force
// live todos" remind): a step is several reads and an edit, not three calls.
// NOTE: a fixed knob, not config; every todos write resets the count.
const todoNagAfter = 8

// maybeNagTodos nudges a stale in_progress todo, once per todos write: the
// model is free to forget, and the next write re-arms the remind. It rides
// the model-only steering channel (attachMarker), so it never reads as the
// user's own message. Only the main agent's calls during its turn count:
// a subagent's would arm the nag while nobody can be nudged.
func (h *Host) maybeNagTodos() {
	// a call esc ended arms nothing: PrepareStep would persist the nag for
	// a step that never runs, ahead of the user's next message
	if t := h.turn.Load(); t != nil && t.ctx.Err() != nil {
		return
	}
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if !ag.active || ag.todoInProgress == "" || ag.todoPending {
		return
	}
	ag.todoCalls++
	if ag.todoCalls < todoNagAfter {
		return
	}
	ag.todoPending = true
	ag.steering = append(ag.steering, modelNote(fmt.Sprintf(
		"Some todos are still unfinished: %q has been in_progress for %d tool calls. If that step is done, tick it now with the todos tool and set the next one in_progress; otherwise keep it in_progress and continue.",
		ag.todoInProgress, ag.todoCalls)))
}

// resetTodoNag re-arms the staleness tracker from a fresh todo write. A
// next turn's first todos write is what started counting, so this also stops
// the nag whenever the model engages at all.
func (h *Host) resetTodoNag(todos []store.Todo) {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	ag.todoInProgress, ag.todoCalls, ag.todoPending = "", 0, false
	if len(todos) < 2 {
		return
	}
	for _, td := range todos {
		if td.Status == "in_progress" {
			ag.todoInProgress = td.Content
			return
		}
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
		return errors.New("agent: session store unavailable")
	}
	sid := store.NewID()
	name := ""
	if p != nil {
		name = p.Name
	}
	first, _, _ := strings.Cut(strings.TrimSpace(firstText), "\n")
	title := truncRunes(first, 60)
	if err := h.DB.CreateSession(sid, "", title, name); err != nil {
		ag.mu.Unlock()
		return err
	}
	ag.sessionID = sid
	ag.titled, ag.firstUser = false, firstText
	h.recentStale.Store(true)
	ag.digest = h.digestFor()
	ag.env = ""
	ag.memAuto = 0
	ag.mu.Unlock()
	h.refreshPending()
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
	Active     bool
	Text       string
	Reasoning  string
	Tools      []StreamTool
	Model      string
	Profile    string
	Steering   []string      // queued messages as shown (cut at attachMarker), oldest first
	Reports    int           // background reports in Steering, which AgentTakeQueue leaves queued
	Thought    time.Duration // wall time spent in reasoning blocks
	Elapsed    time.Duration // whole-turn wall time (0 outside a turn)
	RetryIn    time.Duration // time until the next provider retry (0 when none waits)
	Retries    int           // provider retries this turn
	RetryWhy   string        // what failed, for the retry countdown
	Compacting bool          // history is being summarized before the turn

	// usage tally (DESIGN §8 statusline): session totals, the last prompt
	// size (the current-context estimate), and the context window used for
	// the percent readout.
	SessionIn   int64
	SessionOut  int64
	LastIn      int64
	CtxWindow   int64
	SessionCost float64
	// prompt cache tokens read, over the input billed alongside them. Both
	// count this process only (a resumed session's earlier turns are not in
	// the store), so the share stays honest.
	CacheRead   int64
	CacheBilled int64
}

func (h *Host) AgentStream() StreamSnapshot {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
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
		Profile: h.Settings.Str("agent.profile"),
		Thought: thought, Elapsed: elapsed,
		SessionIn: ag.sessIn, SessionOut: ag.sessOut, LastIn: ag.lastIn,
		CacheRead: ag.cacheRead, CacheBilled: ag.cacheBilled,
		CtxWindow: ag.ctxWin,
	}
	out.Model = ag.modelSeed
	if out.Model == "" {
		out.Model = h.liveModelID()
	}
	if out.CtxWindow == 0 {
		out.CtxWindow = h.contextWindow(out.Model)
	}
	for _, m := range ag.steering {
		shown, _, report := strings.Cut(m, attachMarker)
		out.Steering = append(out.Steering, shown)
		if report {
			out.Reports++
		}
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
	ag.retries, ag.retryAt, ag.compacting = s.Retries, time.Time{}, s.Compacting
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
	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		if h.RT.CurrentOrigin(L) != lua.OriginUser {
			L.RaiseError("zemi.provider.add: only user config may register providers")
			return 0
		}
		spec, err := providerSpecFromLua(L, 1)
		if err != nil {
			L.RaiseError("zemi.provider.add: %v", err)
			return 0
		}
		if err := h.Providers.Register(spec, rawString(L.CheckTable(1), "api_key")); err != nil {
			L.RaiseError("zemi.provider.add: %v", err)
			return 0
		}
		return 0
	}))
	tb.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		if h.RT.CurrentOrigin(L) != lua.OriginUser {
			L.RaiseError("zemi.provider.del: only user config may remove providers")
			return 0
		}
		h.Providers.Delete(L.CheckString(1))
		return 0
	}))
	kinds := L.NewTable()
	kinds.RawSetString("ANTHROPIC", glua.LString(agent.KindAnthropic))
	kinds.RawSetString("OPENAI", glua.LString(agent.KindOpenAI))
	kinds.RawSetString("OPENAI_COMPAT", glua.LString(agent.KindOpenAICompat))
	kinds.RawSetString("GOOGLE", glua.LString(agent.KindGoogle))
	tb.RawSetString("kind", kinds)
	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		for _, s := range h.Providers.List() {
			row := L.NewTable()
			row.RawSetString("id", glua.LString(s.ID))
			row.RawSetString("kind", glua.LString(s.Kind))
			row.RawSetString("base_url", glua.LString(s.BaseURL))
			row.RawSetString("builtin", glua.LBool(s.Builtin))
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
		return agent.Spec{}, errors.New("spec must be a table")
	}
	if name := rawString(tb, "api_key_env"); name != "" {
		return agent.Spec{}, fmt.Errorf(`api_key_env is gone: pass the key itself, api_key = zemi.env.get("%s")`, name)
	}
	spec := agent.Spec{
		ID:      rawString(tb, "id"),
		Kind:    rawString(tb, "kind"),
		BaseURL: rawString(tb, "base_url"),
		Headers: rawStringMap(tb, "headers"),
	}
	if t, ok := tb.RawGetString("body").(*glua.LTable); ok {
		v, err := lua.ToGo(t)
		if err != nil {
			return agent.Spec{}, fmt.Errorf("body: %w", err)
		}
		switch b := v.(type) {
		case map[string]any:
			if !slices.Contains(agent.KindsWithBody, spec.Kind) {
				return agent.Spec{}, fmt.Errorf("body is sent only by the %s kinds", strings.Join(agent.KindsWithBody, " and "))
			}
			spec.Body = b
		case []any:
			if len(b) > 0 {
				return agent.Spec{}, errors.New("body must be a table of named fields")
			}
		}
	}
	models, ok := tb.RawGetString("models").(*glua.LTable)
	if ok {
		models.ForEach(func(_, mv glua.LValue) {
			if mt, ok := mv.(*glua.LTable); ok {
				positive := func(key string) float64 {
					if n := rawNumber(mt, key); n > 0 {
						return n
					}
					return 0
				}
				m := agent.Model{
					ID:                 rawString(mt, "id"),
					Display:            rawString(mt, "display"),
					ContextWindow:      int64(positive("context_window")),
					MaxOutput:          int64(positive("max_output")),
					CostPer1MIn:        positive("cost_per_1m_in"),
					CostPer1MOut:       positive("cost_per_1m_out"),
					CostPer1MInCached:  positive("cost_per_1m_in_cached"),
					CostPer1MOutCached: positive("cost_per_1m_out_cached"),
				}
				m.TextOnly = mt.RawGetString("images") == glua.LFalse
				spec.Models = append(spec.Models, m)
			}
		})
	}
	if spec.ID == "" || spec.Kind == "" {
		return agent.Spec{}, errors.New("provider needs id and kind")
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
		h.refuseBelowUser(L, "model.set")
		id := L.CheckString(1)
		if err := h.modelSetErr(id); err != nil {
			L.RaiseError("zemi.model.set: %v", err)
			return 0
		}
		fired, err := h.Settings.SetQuiet(h.ConversationKey(), id)
		if err != nil {
			L.RaiseError("zemi.model.set: %v", err)
			return 0
		}
		h.deliverFired(L, fired)
		h.warnUnlisted(id)
		return 0
	}))
	tb.RawSetString("pick", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.model.pick")
		_ = h.OpenModelPicker() // headless there is nothing to pick with
		return 0
	}))
	return tb
}

// OpenModelPicker lists the registered models; enter runs /model <id>.
func (h *Host) OpenModelPicker() error {
	ui := h.hooks()
	if ui == nil {
		return errors.New("no UI")
	}
	p, _ := h.resolveProfile("")
	cur, sel := h.currentModelID(p), 0 // what the next send reads, not the running turn's
	var items []any
	for _, s := range h.Providers.List() {
		for _, m := range s.Models {
			id := s.ID + "/" + m.ID
			item := map[string]any{"text": id, "run": "model " + id}
			if d := modelDesc(id, m); d != "" {
				item["desc"] = d
			}
			switch {
			case id == cur:
				item["key"], sel = "current", len(items)
			case !h.Providers.HasKey(s.ID):
				item["key"] = "no API key"
			}
			items = append(items, item)
		}
	}
	o := Overlay{ID: "model", Title: "Model", Origin: lua.OriginBuiltin, Items: items, Selected: sel, Empty: "No models yet: " + h.noModelHint(false)}
	prov, _, _ := strings.Cut(cur, "/")
	_, registered := h.Providers.Lookup(prov)
	if _, known := h.modelSpec(cur); cur != "" && !known && registered {
		// config named it by hand: shown so the typo is in view
		o.Items = append([]any{map[string]any{"text": cur, "run": "model " + cur, "key": "current, not in the list", "desc": "the provider decides whether it exists"}}, items...)
	} else if len(items) == 0 {
		// with nothing usable the rows are the builtin providers and their
		// keys, so a user of one finds its name here rather than in the catalog
		for _, m := range h.Providers.Missing() {
			o.Items = append(o.Items, map[string]any{"text": m.ID, "desc": "export " + m.Env, "copy": "export " + m.Env})
		}
		if len(o.Items) > 0 {
			o.Body = "No model yet: export one of these keys, or " + h.addProvider()
		}
	}
	ui.OpenOverlay(o)
	return nil
}

// modelDesc is the picker's second column: display name, context window
// and USD per million tokens in/out, each left out when unknown.
func modelDesc(id string, m agent.Model) string {
	var parts []string
	if m.Display != "" && m.Display != id {
		parts = append(parts, m.Display)
	}
	switch w := m.ContextWindow; {
	case w >= 1_000_000:
		parts = append(parts, strconv.FormatFloat(float64(w)/1e6, 'g', 3, 64)+"M")
	case w > 0:
		parts = append(parts, strconv.FormatInt(w/1000, 10)+"k")
	}
	if m.CostPer1MIn > 0 || m.CostPer1MOut > 0 {
		parts = append(parts, "$"+strconv.FormatFloat(m.CostPer1MIn, 'g', 3, 64)+"/$"+strconv.FormatFloat(m.CostPer1MOut, 'g', 3, 64))
	}
	return strings.Join(parts, " · ")
}

// ConversationKey is the tier /model and -m write: the smart one when
// config set it, since that is what the main conversation reads, else main.
func (h *Host) ConversationKey() string {
	if h.Settings.Str("models.smart") != "" {
		return "models.smart"
	}
	return "models.main"
}

// SetModel switches the conversation's tier (/model <id>) from any
// goroutine: the config watches it fires run later on the interpreter queue.
func (h *Host) SetModel(id string) error {
	if err := h.modelSetErr(id); err != nil {
		return err
	}
	if err := h.setWatched(h.ConversationKey(), id); err != nil {
		return err
	}
	h.warnUnlisted(id)
	return nil
}

// warnUnlisted toasts a pick the provider does not list. It reads the pick,
// not the model row: mid-turn the row is the running turn's model.
func (h *Host) warnUnlisted(id string) {
	if _, ok := h.modelSpec(id); !ok {
		h.Notify("warn", "Model", unlistedDetail(id))
	}
}

// modelSetErr refuses a model pick under a provider nobody registered, or
// one the active profile would hide. An id the provider does not list is
// taken and warned about, like -m and config: the provider decides.
func (h *Host) modelSetErr(id string) error {
	prov, _, _ := strings.Cut(id, "/")
	if _, ok := h.Providers.Lookup(prov); !ok {
		return h.modelErr(id)
	}
	return h.ProfileModelErr()
}

// WarnModel toasts the model row's warning after a reload, if any: a
// config the provider does not list would otherwise run silently until the
// provider refuses it.
func (h *Host) WarnModel() {
	if w := h.ModelWarning(); w != "" {
		h.Notify("warn", "Model", w)
	}
}

// ProfileModelErr refuses a tier write the active profile would hide: the
// tier would change and the conversation would not.
func (h *Host) ProfileModelErr() error {
	if p, _ := h.resolveProfile(""); p != nil && p.Model != "" {
		return fmt.Errorf("profile %s sets its model; /profile default or drop model from it", p.Name)
	}
	return nil
}

// setWatched writes a config key from any goroutine: the watches it fires
// run later on the interpreter queue.
func (h *Host) setWatched(path, val string) error {
	fired, err := h.Settings.SetQuiet(path, val)
	if err != nil {
		return err
	}
	if len(fired) > 0 {
		h.GoSafe("config watches", func() {
			_, _ = h.RT.Do(context.Background(), lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
				h.deliverFired(L, fired)
				return nil, nil
			})
		})
	}
	h.refresh()
	return nil
}

// ProfileNames lists default, the shipped planner, and the defined profiles.
func (h *Host) ProfileNames() []string {
	h.agents.mu.Lock()
	defer h.agents.mu.Unlock()
	return append([]string{"default", "planner"}, slices.Sorted(maps.Keys(h.agents.profiles))...)
}

// SetProfile switches the profile the next send runs as (/profile <name>).
// Picking a read-only agent remembers the one it replaced, so approving a
// plan can return to it (approvePlan).
func (h *Host) SetProfile(name string) error {
	if _, err := h.resolveProfile(name); err != nil {
		return fmt.Errorf("unknown profile %q (want %s)", name, strings.Join(h.ProfileNames(), ", "))
	}
	next, _ := h.resolveProfile(name)
	if next != nil && next.ReadOnly && !h.readOnly() {
		h.modes.mu.Lock()
		h.modes.prePlanProfile = h.Settings.Str("agent.profile")
		h.modes.prePlanMode = h.Settings.Str("permissions.mode")
		h.modes.mu.Unlock()
	}
	return h.setWatched("agent.profile", name)
}

// cycleProfile steps through the agents (a key binding, like cyclePermMode):
// default, planner, then profiles in definition order, and back.
func (h *Host) cycleProfile(step int) error {
	names := h.ProfileNames()
	n := len(names)
	cur := h.Settings.Str("agent.profile")
	i := slices.Index(names, cur)
	if i < 0 {
		i = 0
	}
	return h.SetProfile(names[((i+step)%n+n)%n])
}

// implementNow is the message a fresh send carries to the restored agent after
// its plan is approved; the plan itself stays in history.
var implementNow = modelNote("The user approved the plan above. Implement it: track its steps with todos, carry them through, verify, and report.")

func (h *Host) setPlanApproved(msg string) {
	h.agents.mu.Lock()
	h.agents.planApproved = msg
	h.agents.mu.Unlock()
}

func (h *Host) takePlanApproved() string {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	msg := ag.planApproved
	ag.planApproved = ""
	return msg
}

// planNudge is what a read-only agent hears once when it ends in plain text
// instead of the question or plan tool: the text is not a plan to approve
// (a question, a report or a refusal would get an approve key), and only
// the tools give the user something to answer in place.
var planNudge = modelNote("Your reply ended in plain text, which the user cannot approve or answer in place. If it asked something, ask it with the question tool; if it was the plan, present it with the plan tool.")

// planFollowup runs after a turn goroutine ends. An approved plan left for
// a cancelled turn (nextTurn hands off an ended one) is sent to the restored
// agent to implement; otherwise a read-only agent that ended in plain text is
// nudged once per user message. Runs on the turn goroutine, never the UI one.
func (h *Host) planFollowup(runner *agent.Runner) {
	if msg := h.takePlanApproved(); msg != "" {
		h.startImplementation(msg)
		return
	}
	if !h.readOnly() || plainTextEnd(runner) == "" {
		return
	}
	// headless drops the plan tool and planHeadless asks for the plan as
	// the reply, so there is nothing to present it with
	if p, _ := h.resolveProfile(""); !hasTool(h.profileTools(p), tools.ToolPlan) {
		return
	}
	// a submit that claimed the agent first moves the conversation on
	ag := h.agents
	ag.mu.Lock()
	skip := ag.active || ag.planNudged
	if !skip {
		ag.planNudged = true
	}
	ag.mu.Unlock()
	if skip {
		return
	}
	if err := h.AgentSend(planNudge); err != nil {
		h.Log.Error("agent", "plan nudge send failed", map[string]any{"error": err.Error()})
	}
}

// startImplementation sends the approved-plan handoff to the restored agent.
func (h *Host) startImplementation(msg string) {
	if err := h.AgentSend(msg); err != nil {
		h.Log.Error("agent", "implementation send failed", map[string]any{"error": err.Error()})
	}
}

// plainTextEnd returns the final plain-text reply of an idle run, or "" when
// it ended in a tool call (an approved or dismissed plan). A read-only agent
// is told to end every turn with the plan tool; one that ended in text forgot.
func plainTextEnd(runner *agent.Runner) string {
	h := runner.History
	if len(h) == 0 || h[len(h)-1].Role != fantasy.MessageRoleAssistant {
		return ""
	}
	var text strings.Builder
	for _, p := range h[len(h)-1].Content {
		if _, ok := p.(fantasy.ToolCallPart); ok {
			return ""
		}
		if tp, ok := p.(fantasy.TextPart); ok {
			text.WriteString(tp.Text)
		}
	}
	return strings.TrimSpace(text.String())
}

// OpenProfilePicker lists the profiles with what each changes; enter switches.
func (h *Host) OpenProfilePicker() error {
	ui := h.hooks()
	if ui == nil {
		return errors.New("no UI")
	}
	cur, sel := h.Settings.Str("agent.profile"), 0
	var items []any
	for i, name := range h.ProfileNames() {
		item := map[string]any{"text": name, "run": "profile " + name, "desc": "the shipped prompt and every tool"}
		if name == "planner" {
			item["desc"] = "read-only: plan and present for approval"
		} else if p, _ := h.resolveProfile(name); p != nil {
			item["desc"] = profileDesc(p)
		}
		if name == cur {
			item["key"], sel = "current", i
		}
		items = append(items, item)
	}
	ui.OpenOverlay(Overlay{ID: "profile", Title: "Profile", Origin: lua.OriginBuiltin, Items: items, Selected: sel, Anchor: "bottom"})
	return nil
}

// profileDesc says in words what a profile changes, for its picker row.
func profileDesc(p *Profile) string {
	tools := "every tool"
	if len(p.Tools) > 0 {
		tools = "only " + strings.Join(p.Tools, ", ")
		if !slices.ContainsFunc(p.Tools, func(t string) bool { return !strings.HasPrefix(t, "-") }) {
			tools = "every tool but " + strings.ReplaceAll(tools[len("only -"):], ", -", ", ")
		}
	}
	if p.Model != "" {
		return p.Model + " - " + tools
	}
	return tools
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
		h.refuseBelowUser(L, "agent.profile")
		p := &Profile{Name: name, Model: rawString(spec, "model"), System: rawString(spec, "system"), ReadOnly: rawBool(spec, "read_only")}
		if temp, ok := spec.RawGetString("temperature").(glua.LNumber); ok {
			t := float64(temp)
			p.Temperature = &t
		}
		p.Tools = rawStrings(spec, "tools")
		if ctx, ok := spec.RawGetString("context").(*glua.LTable); ok {
			p.ContextFiles = rawStrings(ctx, "files")
		}
		h.agents.mu.Lock()
		h.agents.profiles[name] = p
		h.agents.mu.Unlock()
		return 0
	}))

	tb.RawSetString("current", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		ag := h.agents
		ag.mu.Lock()
		out.RawSetString("profile", glua.LString(h.Settings.Str("agent.profile")))
		sid, model := ag.sessionID, h.liveModelID()
		ag.mu.Unlock()
		out.RawSetString("id", glua.LString(sid))
		out.RawSetString("model", glua.LString(model))
		L.Push(out)
		return 1
	}))

	// prompt() is the system prompt the last send assembled, "" before the
	// first one; it is never rebuilt here (see SystemPrompt).
	tb.RawSetString("prompt", L.NewFunction(func(L *glua.LState) int {
		L.Push(glua.LString(h.SystemPrompt()))
		return 1
	}))

	// send returns at once: starting a turn builds the system prompt, which
	// runs agent.assemble handlers on this interpreter and must not wait on it.
	// send(text) -> true, or nil, err. The turn itself must not run on the
	// interpreter (DESIGN §4), so what it reports here is what can be known
	// before it starts; later failures reach the transcript.
	tb.RawSetString("send", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.agent.send")
		text := L.CheckString(1)
		h.refuseBelowUser(L, "agent.send")
		p, err := h.resolveProfile("")
		if err == nil {
			_, err = h.resolveModel(p)
		}
		if err != nil {
			L.Push(glua.LNil)
			L.Push(glua.LString(err.Error()))
			return 2
		}
		h.GoSafe("zemi.agent.send", func() {
			if err := h.AgentSend(text); err != nil {
				h.Log.Warn("agent", "zemi.agent.send failed", map[string]any{"error": err.Error()})
				h.Notify("error", "Could not send", err.Error())
			}
		})
		L.Push(glua.LTrue)
		return 1
	}))

	tb.RawSetString("cancel", L.NewFunction(func(L *glua.LState) int {
		h.AgentCancel()
		// the UI hands a cancelled turn's queue back to the composer; from
		// Lua nobody does, and leaving it would replay those messages into
		// a later turn as if they had just been typed
		if q := h.AgentTakeQueue(); len(q) > 0 {
			if ui := h.hooks(); ui != nil {
				ui.ComposerEdit("set", strings.Join(q, "\n"), -1)
			}
		}
		return 0
	}))

	tb.RawSetString("usage", L.NewFunction(func(L *glua.LState) int {
		s := h.AgentStream()
		out := usageTable(L, s)
		contextTable(L, s).ForEach(func(k, v glua.LValue) { out.RawSetString("context_"+k.String(), v) })
		L.Push(out)
		return 1
	}))

	h.agentRunFns(L, tb)
	return tb
}

func (h *Host) permissionNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	rule := L.NewTable()
	rule.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		spec, ok := L.Get(1).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.permission.rule.add: rule must be a table")
			return 0
		}
		r := perm.Rule{
			Subject:  rawString(spec, "subject"),
			Pattern:  rawString(spec, "pattern"),
			Decision: perm.Decision(rawString(spec, "decision")),
			NotMatch: rawString(spec, "not_match"),
		}
		switch rawString(spec, "scope") {
		case "", "session":
		case "permanent":
			L.RaiseError(`zemi.permission.rule.add: scope "permanent" was removed; a rule in init.lua applies on every start, so drop the scope field`)
		default:
			L.RaiseError(`zemi.permission.rule.add: scope must be "session"`)
		}
		// Origins (DESIGN §5, §6.1): allow rules widen, so they come from
		// user config only; every other origin may only add deny/ask rules.
		if origin := h.RT.CurrentOrigin(L); origin != lua.OriginUser {
			if r.Decision == perm.Allow {
				L.RaiseError("zemi.permission.rule.add: allow rules may only come from user config")
				return 0
			}
			r.Source = string(origin)
		}
		if err := validateRule(&r, rawString(spec, "match"), rawString(spec, "not_match_example")); err != nil {
			L.RaiseError("zemi.permission.rule.add: %v", err)
			return 0
		}
		h.addPermissionRule(r)
		return 0
	}))

	tb.RawSetString("mode", h.modeNamespace(L))

	rule.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		for _, r := range h.Perm.Rules() {
			row := L.NewTable()
			row.RawSetString("subject", glua.LString(r.Subject))
			row.RawSetString("pattern", glua.LString(r.Pattern))
			row.RawSetString("decision", glua.LString(r.Decision))
			row.RawSetString("not_match", glua.LString(r.NotMatch))
			out.Append(row)
		}
		L.Push(out)
		return 1
	}))
	tb.RawSetString("rule", rule)

	return tb
}

// knownSubject: "bash" is not a subject, "tool.bash" is; a rule outside the
// namespaces would sit in the store and never match.
func knownSubject(subject string) bool {
	return slices.ContainsFunc(perm.SubjectPrefixes, func(p string) bool { return strings.HasPrefix(subject, p) })
}

func badSubject(subject string) error {
	return fmt.Errorf("subject %q starts with none of %s", subject, strings.Join(perm.SubjectPrefixes, " "))
}

// validateRule checks a Lua rule. Examples are required (DESIGN §9): the match example must match
// the pattern without being excluded, the not_match example must be
// excluded; a guard needs both examples.
func validateRule(r *perm.Rule, matchEx, notMatchEx string) error {
	switch {
	case r.Subject == "" || r.Pattern == "":
		return errors.New("subject and pattern are required")
	case !knownSubject(r.Subject):
		return badSubject(r.Subject)
	case r.Decision != perm.Allow && r.Decision != perm.Ask && r.Decision != perm.Deny:
		return errors.New("decision must be allow, ask or deny")
	}
	if r.NotMatch == "" {
		if matchEx == "" {
			return fmt.Errorf(`rule %s %q needs a match example: add match = "<an argument the pattern should match>"`, r.Subject, r.Pattern)
		}
		if !perm.MatchExample(r.Pattern, matchEx) {
			return fmt.Errorf("match example %q does not match pattern %q", matchEx, r.Pattern)
		}
		return nil
	}
	if matchEx == "" || notMatchEx == "" {
		return fmt.Errorf("rule %s %q with not_match needs both examples: match and not_match_example", r.Subject, r.Pattern)
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
				// the text parts joined: reading a reply must not mean
				// decoding the provider's wire shape by hand
				var msg fantasy.Message
				if json.Unmarshal([]byte(m.PartsJSON), &msg) == nil {
					row.RawSetString("text", glua.LString(concatText(msg.Content)))
				}
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

// toolPostHook emits tool.post after every builtin tool run.
func (h *Host) toolPostHook() func(string, map[string]any, bool, string, []string) {
	return func(name string, input map[string]any, ok bool, output string, changed []string) {
		if !lspQuietTool[name] {
			h.lspDirty()
		}
		h.EmitAsync("tool.post", map[string]any{
			"tool": name, "input": input, "ok": ok,
			"output": truncRunes(output, 4096), "changed": changed,
		})
	}
}

// readLog is the live session's record of the files the model read. It
// outlives the pipeline, which is rebuilt every send, so a file read in one
// turn can be edited in the next. Only the live session is kept: an older
// one cannot be edited, and holding every path of every session would grow
// without end.
func (h *Host) readLog() *sync.Map {
	sid := h.AgentSessionID()
	ag := h.agents
	ag.readMu.Lock()
	defer ag.readMu.Unlock()
	if ag.reads[sid] == nil {
		ag.reads = map[string]*sync.Map{sid: new(sync.Map)}
		ag.instr = map[string]bool{}
	}
	return ag.reads[sid]
}

// seedReads refills a resumed session's read log from its stored rows: the
// restored context still holds the text of every file the conversation
// read or wrote, so refusing the next edit would only buy a re-read. A file
// changed after the row that last read or wrote it is left out, because
// that text is the one the model does not have.
func (h *Host) seedReads(rows []*store.Message) { h.seedReadsInto(h.readLog(), rows) }

func (h *Host) seedReadsInto(seen *sync.Map, rows []*store.Message) {
	touched := map[string]int64{} // path -> the time of the row that last read or wrote it
	for _, row := range rows {
		var m fantasy.Message
		if json.Unmarshal([]byte(row.PartsJSON), &m) != nil {
			continue
		}
		for _, part := range m.Content {
			call, ok := part.(fantasy.ToolCallPart)
			if !ok || !slices.Contains([]string{tools.ToolRead, tools.ToolWrite, tools.ToolEdit, tools.ToolMultiedit}, call.ToolName) {
				continue
			}
			var in struct {
				Path string `json:"path"`
			}
			if json.Unmarshal([]byte(call.Input), &in) != nil || in.Path == "" {
				continue
			}
			touched[in.Path] = row.CreatedAt
		}
	}
	for path, at := range touched {
		// stored times are milliseconds, file times finer: a file written in
		// the row's own millisecond is not a later change
		fi, err := os.Stat(path)
		if err != nil || fi.ModTime().After(time.UnixMilli(at).Add(time.Millisecond)) {
			continue
		}
		seen.Store(perm.CanonPath(path), fi.ModTime())
	}
}

// afterWrite is the pipeline's AfterWrite hook. The recipe check comes first:
// it may rewrite the file.
func (h *Host) afterWrite(path string) string {
	note := h.recipeAfterWrite(path)
	h.noteWrite(path)
	return note + h.lspAfterWrite(path)
}

// PipelineFor builds the tools pipeline for the model's tool calls. It is
// built per send (profileTools -> ensureRunner), so the read-only snapshot
// below matches the profile the system prompt was built for; switching the
// agent mid-turn changes the next send, not the running one.
func (h *Host) PipelineFor() *tools.Pipeline {
	readOnly := h.readOnly()
	p := &tools.Pipeline{
		Store:       h.Perm,
		Pre:         h.toolPreHook(),
		Ask:         h.askHook(),
		Checkpoint:  h.checkpointFile,
		AfterWrite:  h.afterWrite,
		Changes:     h.bashChanges,
		Progress:    func(id, line string) { h.setToolDetail(id, sanitize.String(line)) },
		Diagnostics: h.lspDiagnostics,
		LSP:         h.lspOpen,
		Plan:        func() bool { return readOnly },
		Sandbox: func() *sandbox.Policy {
			// read-only is absolute: a planner must not touch the project even
			// under yolo. Otherwise yolo allows everything and runs unconfined.
			// A policy with no backend fails every wrap, so without one the
			// planner keeps only Check's denies and asks.
			if !h.sandboxLive() {
				return nil
			}
			if readOnly {
				p := h.sandboxPolicy(true)
				return &p
			}
			if def := h.permModeDef(); def.Base != "yolo" {
				p := h.sandboxPolicy(false)
				return &p
			}
			return nil
		},
		SandboxParam: h.sandboxLive() && h.hooks() != nil,
		Guard:        h.guardSandbox,
		Denied: func(tool string, req tools.Request, v tools.Verdict) {
			h.EmitAsync("tool.denied", map[string]any{"tool": tool, "subject": req.Subject, "arg": req.Arg, "by": v.DecidedBy, "reason": v.Reason})
		},
		Post:      h.toolPostHook(),
		Principal: "model",
		Jobs:      h.jobs,
		Skills:    h.skillIndex,
		Images: func() bool {
			h.agents.mu.Lock()
			id := h.liveModelID()
			h.agents.mu.Unlock()
			return h.takesImages(id)
		},
		Remember: func(scope, text, quote, replaces string) (string, error) {
			return h.memoryTool(h.AgentSessionID(), scope, text, quote, replaces)
		},
		Scratch:      h.inScratch,
		Protected:    func(p string) bool { return !h.inScratch(p) && h.protectedWrite(p) },
		Secret:       h.secretPath,
		ScratchDir:   h.ScratchDir,
		SearchURL:    func() string { return h.Settings.Str("web.search_url") },
		Seen:         h.readLog(),
		Instructions: func(path string) string { return h.nearestInstructions(path, h.instructionSent) },
	}
	if ui := h.hooks(); ui != nil {
		p.Question = ui.AskQuestion
		p.ApprovePlan = h.approvePlan
	}
	if h.DB != nil {
		p.TodosRead = func() ([]store.Todo, error) { return h.DB.Todos(h.AgentSessionID()) }
		p.TodosWrite = func(todos []store.Todo) error {
			sid := h.AgentSessionID()
			if sid == "" {
				return errors.New("no active session")
			}
			defer h.todosStale.Store(true)
			if err := h.DB.SetTodos(sid, todos); err != nil {
				return err
			}
			h.resetTodoNag(todos)
			return nil
		}
	}
	return p
}
