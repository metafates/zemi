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
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/logging"
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
// the pipeline denies file changes and confines commands (see h.readOnly).
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
	Tool string // the asking tool (bash, edit, mcp_github_search)
	// Always is what the session-long choice allows ("allow go test"),
	// "" when none is offered
	Always string
	// Wider is a second session-long choice covering more than Always (a
	// read's whole repository), "" when there is none
	Wider string
	// Project is a persistent choice covering Always's scope but written to
	// the project's permissions file (a trusted bash command), "" when none
	Project string
	Subject string
	Arg     string
	Reason  string
	Preview string // a file tool's unified diff, a script, a memory's text
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
	Status  CallStatus
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
	// the stream state and transcript length when the current step began: a
	// provider retry streams the step again from there
	stepText, stepReason string
	stepTools            []StreamTool
	stepRows             int
	stepThought          time.Duration
	tools                []StreamTool
	steering             []string
	modelSeed            string
	turnModel            string // model id the runner was built with, "" for a bare one: its price, and compact's cached prefix

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

	// readMu guards reads, instr and loaded, the per-session bookkeeping
	// the tools carry (what the model has read, which instruction files it
	// has been given, which MCP tools tool_search loaded). All are dropped
	// when the session changes.
	readMu sync.Mutex
	reads  map[string]*sync.Map
	instr  map[string]bool
	loaded *sync.Map

	// promptTokens estimates the system prompt and tool schemas the runner
	// carries; every prompt holds them, so the context estimate counts them.
	// promptParts is the same total broken down for /usage, promptText the
	// system prompt itself for /prompt.
	promptTokens int64
	promptParts  []promptPart
	promptText   string

	// reasoningShown is the session-scoped expansion override for finished
	// reasoning blocks (nil = follow ui.reasoning config).
	reasoningShown *bool

	// turn timing: turnStart pins the wall clock at AgentSend, thinking
	// tracks the open reasoning block, thought accumulates closed ones
	// since the last flush and stepThinking those of the current step.
	turnStart    time.Time
	thinkStart   time.Time
	thinking     bool
	thought      time.Duration
	stepThinking time.Duration

	// retryAt is when the next provider retry fires, zero again once the
	// retried call streams; retries counts them this turn (the working row
	// counts down instead of looking stuck).
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
	todoSteps      int  // main-runner steps since the last todos write
	todoTouched    bool // a todos call ran this step
	todoPending    bool
	// todoNag waits for the next step boundary of the running turn, outside
	// steering: a turn that ends first drops it instead of starting another.
	todoNag string

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

// Effort is the reasoning level modelID runs at under the agent.effort pick.
func (h *Host) Effort(modelID string) string {
	return h.Providers.Effort(modelID, agent.Effort(h.Settings.Str("agent.effort")))
}

// resolveProfile returns the profile to run: explicit name, else the
// agent.profile key. A defined profile wins; else default is the implicit
// shipped shape (nil) and planner the shipped read-only agent.
func (h *Host) resolveProfile(name string) (*Profile, error) {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if name == "" {
		name = h.Settings.Str("agent.profile")
	}
	if p, ok := ag.profiles[name]; ok {
		return p, nil
	}
	if name == "planner" {
		return &Profile{Name: "planner", ReadOnly: true}, nil
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
// override, else the chat role.
func (h *Host) currentModelID(p *Profile) string {
	if p != nil && p.Model != "" {
		return h.modelRef(p.Model)
	}
	return h.roleModel("chat")
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
	_, loaded := h.sessionLogs()
	toolset, active := h.deferMCP(h.profileTools(p), loaded)
	sys, parts := h.buildSystemParts(p, toolset)
	r := agent.NewRunner(model, toolset, sys)
	r.ActiveTools = active
	sent := toolset
	if active != nil {
		sent, _ = filterTools(toolset, active(), nil)
	}
	sizes := promptSizes(parts, sent)
	chars := 0
	for _, s := range sizes {
		chars += s.chars
	}
	h.agents.mu.Lock()
	h.agents.promptParts, h.agents.promptTokens, h.agents.promptText = sizes, int64(chars/4), sys
	h.agents.mu.Unlock()
	id := h.currentModelID(p)
	sid := h.AgentSessionID()
	r.ProviderOptions, r.Headers = h.Providers.CallOptions(id, h.Effort(id), sid), agent.SessionHeaders(sid)
	r.MaxOutputTokens = h.maxOutput(id)
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
// (tool_search included) apart since what it sends rides every request
// whether the model calls it or not.
func promptSizes(p promptParts, toolset []fantasy.AgentTool) []promptPart {
	builtin, mcp := 0, 0
	for _, t := range toolset {
		info := t.Info()
		schema, _ := json.Marshal(info.Parameters)
		n := len(info.Name) + len(info.Description) + len(schema)
		if strings.HasPrefix(info.Name, mcpToolPrefix) || info.Name == tools.ToolSearch {
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
		h.Notify(logging.Error, "Could not send", h.friendlyError(err))
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

	// the profile this turn ran as: a switch to the planner mid-turn must not
	// nudge the reply the replaced agent wrote
	nudge := h.nudgesPlan(p)
	h.GoSafe("agent turn", func() {
		h.runTurns(runner, scope, text)
		h.planFollowup(runner, nudge)
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
			h.Notify(logging.Error, "Could not send", h.friendlyError(err))
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
	if h.appendUserRows(text) {
		h.agents.mu.Lock()
		h.agents.planNudged = false
		h.agents.mu.Unlock()
	}
	h.agentStream()
	h.EmitAsync("agent.start", map[string]any{"text": text})
}

// appendUserRows shows one message as the model reads it: the notice row
// of each background report behind agent.AttachMarker, then what was typed, the
// same cut a resumed transcript makes. Reports whether anything was typed.
func (h *Host) appendUserRows(text string) bool {
	shown, tail, _ := strings.Cut(text, agent.AttachMarker)
	for _, head := range reportHeads(tail) {
		h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: head})
	}
	if shown == "" {
		return false
	}
	h.AppendMessage(fantasy.MessageRoleUser, shown)
	return true
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
	var waiting []string // reports and plugin sends a cancel left for the next message
	if done.ctx.Err() != nil {
		for _, m := range ag.steering {
			_, tail, _ := strings.Cut(m, agent.AttachMarker)
			waiting = append(waiting, reportHeads(tail)...)
		}
	} else {
		if ag.planApproved != "" {
			ag.steering = append(ag.steering, ag.planApproved)
			ag.planApproved, handoff = "", true
		}
		text = joinQueue(ag.steering)
		ag.steering = nil
	}
	if ag.todoNag != "" {
		ag.todoNag, ag.todoPending = "", false
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
		// its notice row is drawn when the model reads it; say now that it waits
		if len(waiting) > 0 {
			h.Notify(logging.Info, "Waiting for your next message", strings.Join(waiting, "\n"))
		}
		h.refresh()
	case handoff:
		if err := h.sendClaimed(next, text); err != nil {
			h.Notify(logging.Error, "Could not start the implementation", h.friendlyError(err))
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
	msgs := ag.steering
	if ag.todoNag != "" {
		msgs = append(msgs, ag.todoNag)
	}
	if len(msgs) == 0 {
		ag.mu.Unlock()
		return nil
	}
	ag.steering, ag.todoNag = nil, ""
	h.flushOutput(TurnStat{})
	ag.mu.Unlock()
	for _, m := range msgs {
		h.appendUserRows(m)
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
		if strings.Contains(m, agent.AttachMarker) {
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
// of the first agent.AttachMarker, where each reader cuts the text.
func joinQueue(msgs []string) string {
	shown := make([]string, len(msgs))
	var tails strings.Builder
	for i, m := range msgs {
		s, tail, ok := strings.Cut(m, agent.AttachMarker)
		if ok {
			tails.WriteString(agent.AttachMarker + tail)
		}
		shown[i] = s
	}
	return strings.Join(shown, "\n\n") + tails.String()
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
	ag.retryAt = time.Time{}
	ag.textBuf += text
	ag.mu.Unlock()
	s.h.agentStream()
	// NOTE: one queue round trip per delta; fine for the few configs that
	// listen, coalesce here if one ever streams into something slow.
	s.h.EmitAsync("agent.text_delta", map[string]any{"text": text})
}

func (s *streamAdapter) StepStart() {
	rows := len(s.h.Session().Transcript)
	ag := s.h.agents
	ag.mu.Lock()
	// capped, so a pending row appended later cannot write into the saved slice
	ag.stepText, ag.stepReason, ag.stepTools, ag.stepRows, ag.stepThought = ag.textBuf, ag.reasonBuf, slices.Clip(ag.tools), rows, ag.thought
	ag.stepThinking = 0
	ag.mu.Unlock()
}

// Retrying also takes back what a call opened in the failed attempt moved
// into the transcript (openToolRow), or its text shows twice above a failed row.
func (s *streamAdapter) Retrying(err error, delay time.Duration) {
	ag := s.h.agents
	ag.mu.Lock()
	ag.textBuf, ag.reasonBuf, ag.tools, ag.thought, ag.thinking, ag.stepThinking = ag.stepText, ag.stepReason, ag.stepTools, ag.stepThought, false, 0
	rows := ag.stepRows
	ag.retries++
	ag.retryAt = time.Now().Add(delay)
	ag.retryWhy, _ = s.h.explain(err) // the advice is to wait, which the countdown says
	why := ag.retryWhy
	ag.mu.Unlock()
	s.h.cutAssistantRows(rows)
	s.h.Log.Warn("agent", "provider retry", map[string]any{"error": errString(err), "delay_ms": delay.Milliseconds()})
	if s.h.hooks() == nil { // print mode: the countdown is on no screen
		fmt.Fprintf(s.h.Stderr, "zemi: retrying in %s: %s\n", fmtDuration(delay), why)
	}
	s.h.agentStream()
}

// cutAssistantRows drops the assistant rows past the first n. A !command row
// that landed meanwhile stays.
func (h *Host) cutAssistantRows(n int) {
	h.tMu.Lock()
	if t := h.session.Transcript; len(t) > n {
		kept := slices.DeleteFunc(slices.Clone(t[n:]), func(m ChatMessage) bool { return m.Role == fantasy.MessageRoleAssistant })
		// replaced whole, like setTranscript: readers share the old array
		// (Host.Session) and the chat widget caches rows by index
		h.session.Transcript = append(slices.Clip(t[:n]), kept...)
		h.trEpoch.Add(1)
	}
	h.tMu.Unlock()
	h.refresh()
}

func (s *streamAdapter) ReasoningDelta(text string) {
	ag := s.h.agents
	ag.mu.Lock()
	// a new step thinks: what the last one showed goes above it. Reasoning
	// after text with no call between, or while the step's calls are still
	// open, is a late chunk of the same thought (providers send the last
	// token after the reply or the call starts), so it joins the block above
	// and neither the reply nor the thought is cut. Only a finished call
	// closes a thought: a step cannot end while one of its calls runs.
	// NOTE: reasoning a provider interleaves after a call still open in the
	// same step draws above that call live and below it on resume; an
	// ordered live list would fix it.
	if slices.ContainsFunc(ag.tools, func(t StreamTool) bool { return t.Status != CallRun && t.Status != CallWait }) {
		s.h.flushOutput(TurnStat{})
	}
	ag.beginThinking()
	ag.retryAt = time.Time{}
	ag.reasonBuf += text
	ag.mu.Unlock()
	s.h.agentStream()
	s.h.EmitAsync("agent.reasoning_delta", map[string]any{"text": text})
}

// ToolCallPending shows the call while the model is still streaming its
// arguments: a write's file body or a long heredoc takes seconds that a
// spinner alone says nothing about.
func (s *streamAdapter) ToolCallPending(id, name string) {
	s.openToolRow(StreamTool{ID: id, Name: name, Status: CallWait})
}

// inputDiff is a file tool's change as its input describes it, before it
// runs. It reads the file: call it outside the locks UI readers share.
func inputDiff(name, input string) string {
	if name != tools.ToolWrite && name != tools.ToolEdit {
		return ""
	}
	var in map[string]any
	if json.Unmarshal([]byte(input), &in) != nil {
		return ""
	}
	return capLines(tools.ChangeDiff(name, in), 24)
}

func (s *streamAdapter) ToolCallStart(id, name, input string) {
	row := StreamTool{ID: id, Name: name, Input: toolSummary(input), Label: toolLabel(input), Status: CallWait, Diff: inputDiff(name, input)}
	ag := s.h.agents
	ag.mu.Lock()
	i := slices.IndexFunc(ag.tools, func(t StreamTool) bool { return t.ID == id })
	if i < 0 {
		ag.mu.Unlock()
		s.openToolRow(row) // this provider reports no input start
		return
	}
	ag.tools[i] = row
	ag.mu.Unlock()
	s.h.agentStream()
}

// ToolCallRunning starts a row's clock: until then it draws no timer, so a
// call queued behind a serial one does not count time it has not run.
func (s *streamAdapter) ToolCallRunning(id string) {
	ag := s.h.agents
	ag.mu.Lock()
	if i := slices.IndexFunc(ag.tools, func(t StreamTool) bool { return t.ID == id && t.Status == CallWait }); i >= 0 {
		ag.tools[i].Status, ag.tools[i].Started = CallRun, time.Now()
	}
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
	ag.retryAt = time.Time{}
	if strings.TrimSpace(ag.textBuf) != "" {
		s.h.flushOutput(TurnStat{})
	}
	ag.tools = append(ag.tools, row)
	ag.mu.Unlock()
	s.h.agentStream()
}

func (s *streamAdapter) ToolCallEnd(id, name string, ok bool, output, changed string) {
	ag := s.h.agents
	t := s.h.turn.Load()
	status := toolStatus(name, ok, output, t != nil && t.ctx.Err() != nil)
	row := StreamTool{ID: id, Name: name}
	ag.mu.Lock()
	for i := len(ag.tools) - 1; i >= 0; i-- {
		t := &ag.tools[i]
		if t.ID == id && (t.Status == CallRun || t.Status == CallWait) {
			t.Status, t.Output = status, shortOutput(output)
			if !ok {
				t.Diff = "" // the change did not happen
			}
			if changed != "" {
				t.Diff = changed // bash: what the command changed, failed or not
			}
			if name == tools.ToolBash {
				t.Detail = "" // its last output line, which the result now holds
			}
			row = *t
			break
		}
	}
	ag.mu.Unlock()
	s.h.agentStream()
	s.h.recordToolCall(s.h.AgentSessionID(), row, ok)
	if name == tools.ToolTodos {
		ag.mu.Lock()
		ag.todoTouched = true
		ag.mu.Unlock()
	}
}

// toolStatus is a finished call's row status: stopped when the user
// denied, dismissed or interrupted it (cancelled), so the row does not read
// as a failure.
func toolStatus(name string, ok bool, output string, cancelled bool) CallStatus {
	switch {
	case name == tools.ToolAgent && strings.HasPrefix(output, bgAgentStarted),
		name == tools.ToolBash && strings.Contains(output, "so it "+tools.JobHandoff),
		name == tools.ToolJobStart && ok:
		return CallBG
	case ok:
		return CallOK
	case cancelled || tools.UserStopped(output):
		return CallStopped
	}
	return CallErr
}

// StepDone bills a completed step's usage to the live session tally, so the
// token count moves during the turn instead of only at TurnDone, and counts
// the step for the todo nag unless a todos call ran in it.
func (s *streamAdapter) StepDone(st agent.Step) {
	h := s.h
	ag := h.agents
	ag.mu.Lock()
	sid, model, touched := ag.sessionID, ag.turnModel, ag.todoTouched
	ag.todoTouched = false
	ag.mu.Unlock()
	if model == "" {
		model = h.currentModelID(nil) // a turn the host did not start (tests)
	}
	h.bill(sid, model, st.Usage)
	h.emitStep(sid, model, st)
	if !touched {
		h.maybeNagTodos()
	}
}

// emitStep is agent.step, one per model call of a turn, a subagent's included.
func (h *Host) emitStep(sid, model string, st agent.Step) {
	u := st.Usage
	h.EmitAsync("agent.step", map[string]any{
		"session": sid, "model": model, "step": st.N,
		"ms": st.Took.Milliseconds(), "first_token_ms": st.FirstToken.Milliseconds(), "tools": st.Tools,
		"usage": map[string]any{
			"in": u.InputTokens, "out": u.OutputTokens, "cache_read": u.CacheReadTokens, "cache_write": u.CacheCreationTokens,
			"cost": h.turnCost(model, u),
		},
	})
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
	text, tools := ag.textBuf, ag.tools
	stat := TurnStat{In: usage.InputTokens, Out: usage.OutputTokens}
	if !ag.turnStart.IsZero() {
		stat.ElapsedMS = time.Since(ag.turnStart).Milliseconds()
	}
	ag.turnStart = time.Time{}
	// a call still open here was cut off: by the user, or by the error that
	// ended the turn
	for i, t := range ag.tools {
		if t.Status == CallRun || t.Status == CallWait {
			ag.tools[i].Status, ag.tools[i].Diff = map[bool]CallStatus{true: CallStopped, false: CallErr}[errors.Is(err, context.Canceled)], ""
		}
	}
	h.flushOutput(stat)
	sid := ag.sessionID
	ag.mu.Unlock()
	h.refresh()
	if hold {
		return
	}
	ui := h.hooks()
	if ui != nil {
		stopped := errors.Is(err, context.Canceled) || text == "" && slices.ContainsFunc(tools, func(t StreamTool) bool { return t.Status == CallStopped })
		ui.TurnDone(h.friendlyError(err), stopped)
	}
	// a print run exits before a title call answers: no UI, no title
	if err == nil && strings.TrimSpace(text) != "" && ui != nil {
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
		row := ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowError, Text: truncRunes(sanitize.String(h.errorRowText(err)), 400)}
		if errors.Is(err, context.Canceled) {
			row.Status = CallCancelled
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

// flushOutput moves the live output into the transcript in the order it
// streamed: the reasoning, the tool rows that finished, then the reply with
// stat. Calls still running or waiting stay live, so a flush never settles
// one early or draws it twice. Both change under ag.mu, which liveView reads
// them under, so no frame draws a row twice or not at all (ag.mu held).
func (h *Host) flushOutput(stat TurnStat) {
	ag := h.agents
	ag.endThinking()
	var rows []ChatMessage
	if strings.TrimSpace(ag.reasonBuf) != "" {
		rows = append(rows, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowReasoning, Text: ag.reasonBuf, Stat: &TurnStat{ThoughtMS: ag.thought.Milliseconds()}})
	}
	done := slices.IndexFunc(ag.tools, func(t StreamTool) bool { return t.Status == CallRun || t.Status == CallWait })
	if done < 0 {
		done = len(ag.tools)
	}
	for _, t := range ag.tools[:done] {
		rows = append(rows, ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: t.Name, Text: t.Input, Label: t.Label, Status: t.Status, Detail: t.Detail, Run: t.Run, Diff: t.Diff, Output: t.Output})
	}
	if strings.TrimSpace(ag.textBuf) != "" {
		rows = append(rows, ChatMessage{Role: fantasy.MessageRoleAssistant, Text: ag.textBuf, Stat: &stat})
	}
	ag.reasonBuf, ag.tools, ag.textBuf, ag.thought = "", slices.Clone(ag.tools[done:]), "", 0
	h.tMu.Lock()
	h.session.Transcript = append(h.session.Transcript, rows...)
	h.tMu.Unlock()
}

// appendAfterLive adds m to the transcript where the turn has got to: after
// the live rows that finished, which move there first, and above the calls
// still running.
func (h *Host) appendAfterLive(m ChatMessage) {
	ag := h.agents
	ag.mu.Lock()
	h.flushOutput(TurnStat{})
	h.tMu.Lock()
	h.session.Transcript = append(h.session.Transcript, m)
	h.tMu.Unlock()
	ag.mu.Unlock()
	h.refresh()
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
	if len(lines) <= n+1 {
		return s
	}
	return strings.Join(lines[:n], "\n") + "\n" + MoreLines(len(lines)-n)
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
	a, _ := m["agent"].(string)
	if to, _ := m["send_to"].(string); to != "" {
		a = "to " + to
	}
	if a != "" {
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
	// any other tool (an MCP one): its first string argument, in the order
	// the model wrote them
	dec := json.NewDecoder(strings.NewReader(input))
	_, _ = dec.Token() // the object's opening brace
	for dec.More() {
		var v any
		if _, err := dec.Token(); err != nil || dec.Decode(&v) != nil {
			break
		}
		if s, ok := v.(string); ok && s != "" {
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
	// how long the step thought, which the message does not keep: a resume
	// reads it back (historyFrom)
	if slices.ContainsFunc(msg.Content, func(p fantasy.MessagePart) bool { return p.GetType() == fantasy.ContentTypeReasoning }) {
		ag := h.agents
		ag.mu.Lock()
		d := ag.stepThinking
		if ag.thinking {
			d += time.Since(ag.thinkStart)
		}
		ag.mu.Unlock()
		parts, _ := json.Marshal(map[string]any{"thought_ms": d.Milliseconds()})
		if err := h.DB.AddMessage(&store.Message{SessionID: sid, Role: "tool", PartsJSON: string(parts)}); err != nil {
			return err
		}
	}
	if role == string(fantasy.MessageRoleUser) {
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
// as it was, so a resumed session could never rebuild it (historyFrom); so
// is an agent call's run and summary line.
func (h *Host) recordToolCall(sid string, t StreamTool, ok bool) {
	if h.DB == nil || sid == "" {
		return
	}
	row := map[string]any{"tool": t.Name}
	if t.Diff != "" || t.Run != "" {
		row["id"], row["diff"], row["run"], row["detail"] = t.ID, t.Diff, t.Run, t.Detail
	}
	parts, _ := json.Marshal(row)
	msg := &store.Message{SessionID: sid, Role: "tool", PartsJSON: string(parts)}
	if err := h.DB.AddToolCall(msg, &store.ToolCall{Tool: t.Name, OK: ok}); err != nil {
		h.Log.Warn("agent", "tool call persist failed", map[string]any{"error": err.Error()})
	}
}

// todoNagAfter is how many of the main agent's steps an in_progress todo
// may stay static before zemi nudges the model to mark it done (a "force live
// todos" remind). Steps, not calls: the prompt asks for independent calls
// in one batch, so one step may be eight reads.
// NOTE: a fixed knob, not config; every todos write resets the count.
const todoNagAfter = 4

// maybeNagTodos counts a main-runner step against a stale in_progress
// todo and nudges once per todos write: the model is free to forget, and
// the next write re-arms the remind. It rides the model-only steering
// channel (agent.AttachMarker), so it never reads as the user's own message.
// Only the main agent's steps during its turn count: a subagent's would
// arm the nag while nobody can be nudged.
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
	ag.todoSteps++
	if ag.todoSteps < todoNagAfter {
		return
	}
	ag.todoPending = true
	ag.todoNag = agent.Note(fmt.Sprintf(
		"%q has been in_progress for %d steps. If it is done, mark it done with the todos tool and set the next item in_progress; if not, carry on.",
		ag.todoInProgress, ag.todoSteps))
}

// resetTodoNag re-arms the staleness tracker from a fresh todo write. A
// next turn's first todos write is what started counting, so this also stops
// the nag whenever the model engages at all.
func (h *Host) resetTodoNag(todos []store.Todo) {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	ag.todoInProgress, ag.todoSteps, ag.todoPending = "", 0, false
	if len(todos) < 2 {
		return
	}
	for _, td := range todos {
		if td.Status == store.TodoInProgress {
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
	typed, attached := agent.CutAttached(firstText)
	if strings.TrimSpace(typed) == "" {
		// piped stdin alone: its first line, past the element's tag
		_, typed, _ = strings.Cut(strings.TrimPrefix(attached, agent.AttachMarker+"\n"), "\n")
	}
	first, _, _ := strings.Cut(strings.TrimSpace(typed), "\n")
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

// btwPrompt frames a side question, after maki's: nothing it says is kept,
// so the answer has to come from what the conversation already holds.
const btwPrompt = `Side question. Answer only it, in this one reply, from what the conversation already shows. Call no tools: none will run. If the conversation does not tell you, say so.

Question: `

// Btw runs /btw: a side question answered in an overlay from the
// conversation so far, a running turn included, and never stored. Headless
// the answer is a transcript row, which -p prints; the call claims the agent
// like /compact, so -p waits on it and its ctrl+c cancels it. In the TUI it
// is a busy job, which a double esc cancels.
func (h *Host) Btw(question string) error {
	ui := h.hooks()
	if ui == nil {
		ag := h.agents
		ag.mu.Lock()
		if ag.active {
			ag.mu.Unlock()
			return errors.New("a turn is running; ask after it")
		}
		ag.active = true
		scope := h.beginScope()
		ag.mu.Unlock()
		h.GoSafe("btw", func() {
			defer h.endTurnAfterPanic()
			answer, err := h.btw(scope.ctx, question)
			row := ChatMessage{Role: fantasy.MessageRoleAssistant, Text: answer}
			if err != nil {
				row = ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowError, Text: sanitize.String(h.errorRowText(err))}
			}
			h.AppendChat(row)
			h.handOff(scope)
		})
		return nil
	}
	h.GoSafe("btw", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer h.Busy("Answering the side question", cancel)()
		answer, err := h.btw(ctx, question)
		switch {
		case ctx.Err() != nil:
		case errors.Is(err, errNothingToAsk):
			h.Notify(logging.Warn, "Nothing to ask about yet", "")
		case err != nil:
			h.Notify(logging.Error, "Could not answer the side question", h.friendlyError(err))
		default:
			ui.OpenOverlay(Overlay{ID: fmt.Sprintf("btw:%d", h.selectSeq.Add(1)), Title: "Side question", Origin: lua.OriginBuiltin, Body: answer, BodyMD: true})
		}
	})
	return nil
}

var errNothingToAsk = errors.New("no conversation to ask about yet")

// btw sends the question on the prefix the next send would use (profile,
// prompt, tools), so it reads the provider's cache, and bills it.
func (h *Host) btw(ctx context.Context, question string) (string, error) {
	ag := h.agents
	ag.mu.Lock()
	live, sid, compacting := ag.runner, ag.sessionID, ag.compacting
	ag.mu.Unlock()
	switch {
	case compacting:
		return "", errors.New("compacting the conversation; ask after it")
	case live == nil:
		return "", errNothingToAsk
	}
	history := live.Snapshot()
	p, err := h.resolveProfile("")
	if err != nil {
		return "", err
	}
	r, err := h.ensureRunner(p)
	if err != nil {
		return "", err
	}
	res, err := r.Aside(ctx, history, btwPrompt+question)
	if err != nil {
		return "", err
	}
	h.bill(sid, h.currentModelID(p), res.TotalUsage)
	return cmp.Or(strings.TrimSpace(concatText(res.Response.Content)), "(no answer: the model reached for a tool)"), nil
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
	Steering   []string      // queued messages as shown (cut at agent.AttachMarker), oldest first
	Reports    int           // background reports in Steering, which AgentTakeQueue leaves queued
	Thought    time.Duration // wall time spent in reasoning blocks
	Elapsed    time.Duration // whole-turn wall time (0 outside a turn)
	Retrying   bool          // a provider retry waits or is under way
	RetryIn    time.Duration // time until the next provider retry, 0 once it is under way
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
	h.agents.mu.Lock()
	defer h.agents.mu.Unlock()
	return h.streamLocked()
}

// liveView is the transcript and the live turn read together, as
// flushOutput changes them.
func (h *Host) liveView() (Session, StreamSnapshot) {
	h.agents.mu.Lock()
	defer h.agents.mu.Unlock()
	return h.Session(), h.streamLocked()
}

// streamLocked is AgentStream with ag.mu held.
func (h *Host) streamLocked() StreamSnapshot {
	ag := h.agents
	thought := ag.thought
	if ag.thinking {
		thought += time.Since(ag.thinkStart)
	}
	// milliseconds, as the settled row stores it, so both read alike
	thought = thought.Truncate(time.Millisecond)
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
		shown, _, report := strings.Cut(m, agent.AttachMarker)
		out.Steering = append(out.Steering, shown)
		if report {
			out.Reports++
		}
	}
	out.Tools = append(out.Tools, ag.tools...)
	out.Retries, out.Compacting, out.SessionCost = ag.retries, ag.compacting, ag.sessCost
	if !ag.retryAt.IsZero() {
		out.Retrying, out.RetryIn, out.RetryWhy = true, max(time.Until(ag.retryAt), 0), ag.retryWhy
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
	if s.Retrying {
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
		d := time.Since(ag.thinkStart)
		ag.thought += d
		ag.stepThinking += d
		ag.thinking = false
	}
}

// ---------------------------------------------------------------------------
// namespaces

// enumTable is a table of constants keyed by each value upper-cased, like
// zemi.provider.kind.OPENAI_COMPAT.
func enumTable[T ~string](L *glua.LState, vals ...T) *glua.LTable {
	tb := L.CreateTable(0, len(vals))
	for _, v := range vals {
		tb.RawSetString(strings.ToUpper(string(v)), glua.LString(v))
	}
	return tb
}

// strs is a closed set's values as plain strings.
func strs[T ~string](vals []T) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = string(v)
	}
	return out
}

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
	tb.RawSetString("kind", enumTable(L, agent.Kinds...))
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
	tb, ok := L.Get(idx).(*glua.LTable)
	if !ok {
		return agent.Spec{}, errors.New("spec must be a table")
	}
	raw, err := lua.ToGo(tb)
	if err != nil {
		return agent.Spec{}, fmt.Errorf("spec: %w", err)
	}
	fields, _ := raw.(map[string]any)
	if err := onlyKeys(fields, "spec", "id", "kind", "api_key", "base_url", "headers", "body", "models"); err != nil {
		return agent.Spec{}, err
	}
	spec := agent.Spec{
		ID:      rawString(tb, "id"),
		Kind:    agent.Kind(rawString(tb, "kind")),
		BaseURL: rawString(tb, "base_url"),
		Headers: rawStringMap(tb, "headers"),
	}
	switch b := fields["body"].(type) {
	case map[string]any:
		if !slices.Contains(agent.KindsWithBody, spec.Kind) {
			return agent.Spec{}, fmt.Errorf("body is sent only by the %s kinds", strings.Join(strs(agent.KindsWithBody), ", "))
		}
		spec.Body = b
	case []any:
		return agent.Spec{}, errors.New("body must be a table of named fields")
	}
	// an empty table marshals as a map
	models, ok := fields["models"].([]any)
	if m, isMap := fields["models"].(map[string]any); !ok && fields["models"] != nil && (!isMap || len(m) > 0) {
		return agent.Spec{}, errors.New("models must be a list of model entries")
	}
	for i, v := range models {
		at := fmt.Sprintf("models[%d]", i+1)
		e, ok := v.(map[string]any)
		if !ok {
			return agent.Spec{}, fmt.Errorf("%s must be a table", at)
		}
		if err := onlyKeys(e, at, "id", "display", "context_window", "max_output", "vision",
			"cost_per_1m_in", "cost_per_1m_out", "cost_per_1m_cache_write", "cost_per_1m_cache_read"); err != nil {
			return agent.Spec{}, err
		}
		positive := func(key string) float64 {
			n, _ := e[key].(float64)
			return max(n, 0)
		}
		id, _ := e["id"].(string)
		display, _ := e["display"].(string)
		spec.Models = append(spec.Models, agent.Model{
			ID:                  id,
			Display:             display,
			ContextWindow:       int64(positive("context_window")),
			MaxOutput:           int64(positive("max_output")),
			CostPer1MIn:         positive("cost_per_1m_in"),
			CostPer1MOut:        positive("cost_per_1m_out"),
			CostPer1MCacheWrite: positive("cost_per_1m_cache_write"),
			CostPer1MCacheRead:  positive("cost_per_1m_cache_read"),
			TextOnly:            e["vision"] == false,
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
		out, ids := L.NewTable(), h.roleIDs()
		for _, s := range h.Providers.List() {
			for _, m := range s.Models {
				id := s.ID + "/" + m.ID
				var on []string
				for i, r := range roles {
					if ids[i] == id {
						on = append(on, r.Name)
					}
				}
				row := L.NewTable()
				row.RawSetString("id", glua.LString(id))
				row.RawSetString("display", glua.LString(m.Display))
				row.RawSetString("has_key", glua.LBool(h.Providers.HasKey(s.ID)))
				row.RawSetString("roles", lua.FromGo(L, on))
				out.Append(row)
			}
		}
		L.Push(out)
		return 1
	}))
	tb.RawSetString("set", L.NewFunction(func(L *glua.LState) int {
		h.refuseBelowUser(L, "model.set")
		id := L.CheckString(1)
		if err := h.modelSetErr("chat", id); err != nil {
			L.RaiseError("zemi.model.set: %v", err)
			return 0
		}
		if h.cfgReplayed("models.chat", id) {
			return 0
		}
		fired, err := h.Settings.SetQuiet("models.chat", id)
		if err != nil {
			L.RaiseError("zemi.model.set: %v", err)
			return 0
		}
		h.deliverFired(L, fired)
		h.warnUnlisted(id)
		return 0
	}))
	tb.RawSetString("ask", L.NewFunction(h.modelAsk))
	tb.RawSetString("role", enumTable(L, roleNames()...))
	tb.RawSetString("effort", enumTable(L, agent.Efforts...))
	tb.RawSetString("pick", L.NewFunction(func(L *glua.LState) int {
		role := L.OptString(1, "")
		if role != "" && !isRole(role) {
			L.RaiseError("zemi.model.pick: unknown role %q; roles: %s", role, strings.Join(roleNames(), ", "))
			return 0
		}
		_ = h.OpenModelPicker(role) // headless there is nothing to pick with
		return 0
	}))
	return tb
}

// roleIDs is the model id each role runs on, in roles order. chat is
// models.chat even while the active profile runs its own model, since that
// is what every role following chat and a model reference "chat" get.
func (h *Host) roleIDs() []string {
	ids := make([]string, len(roles))
	for i, r := range roles {
		ids[i] = h.roleModel(r.Name)
	}
	return ids
}

// OpenModelPicker lists the registered models for role, where enter sets
// it, then the keyless builtin providers, where it copies the export line.
// Role "" is /model, which sets chat under a row opening /model roles;
// another role's list is a step of /model roles and opens on a row that
// makes the role follow chat.
func (h *Host) OpenModelPicker(role string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	ui.OpenOverlay(h.modelPicker(role))
	return nil
}

func (h *Host) modelPicker(role string) Overlay {
	o := Overlay{ID: "model", Title: "Model", Origin: lua.OriginBuiltin, Anchor: OverlayBottomRight, Verb: "pick", Empty: "No models yet. Add a provider to " + h.initPath() + "."}
	run, cur := "model ", h.roleModel("chat")
	switch role {
	case "":
		o.Items = []any{map[string]any{"text": "Choose a model per role", "desc": rolesDesc(h.ownRoles()), "verb": "open", "run": "model roles"},
			map[string]any{"text": "Update the model catalog", "desc": h.Builtin.Load().Info.String(), "verb": "update", "run": "model update"}}
	case "chat":
		o.ID, o.Title = "model chat", "Model roles / chat"
	default:
		o.ID, o.Title, run, cur = "model "+role, "Model roles / "+role, "model "+role+" ", h.Settings.Str("models."+role)
		head := map[string]any{"text": "Follow the chat model", "desc": h.roleModel("chat"), "run": run + "chat"}
		if cur == "" {
			head["key"] = "current"
		}
		o.Items = []any{head}
	}
	heads := len(o.Items)
	for _, s := range h.Providers.List() {
		section := s.ID
		if !h.Providers.HasKey(s.ID) {
			section += " · no API key"
		}
		for _, m := range s.Models {
			id := s.ID + "/" + m.ID
			item := map[string]any{"text": m.ID, "section": section, "desc": modelDesc(id, m), "run": run + id, "copy": id}
			if id == cur {
				item["key"], o.Selected = "current", len(o.Items)
			}
			o.Items = append(o.Items, item)
		}
	}
	prov, _, _ := strings.Cut(cur, "/")
	_, registered := h.Providers.Lookup(prov)
	if _, known := h.modelSpec(cur); cur != "" && !known && registered {
		// config named it by hand: shown so the typo is in view
		o.Items = slices.Insert(o.Items, heads, any(map[string]any{"text": cur, "run": run + cur, "key": "current, not in the list", "desc": "the provider decides whether it exists"}))
		o.Selected = heads
	}
	usable := len(o.Items) > heads
	if !usable {
		o.Items = nil
	}
	// the builtin providers without a key close the list, so a user finds
	// what else zemi supports and the variable that turns each on
	for _, m := range h.Providers.Missing() {
		o.Items = append(o.Items, map[string]any{"text": m.ID, "section": "Needs a key", "desc": "export " + m.Env, "copy": "export " + m.Env})
	}
	if !usable && len(o.Items) > 0 {
		o.Body = "No models yet. Export one of these keys, or " + h.addProvider() + "."
	}
	return o
}

// ownRoles names the roles config gave a model of their own.
func (h *Host) ownRoles() []string {
	var own []string
	for _, r := range roles[1:] {
		if h.Settings.Str("models."+r.Name) != "" {
			own = append(own, r.Name)
		}
	}
	return own
}

// rolesDesc is the desc of /model's row opening the roles: which roles leave the chat model.
func rolesDesc(own []string) string {
	switch n := len(own); n {
	case 0:
		return "every role on the chat model"
	case 1:
		return own[0] + " on its own model"
	default:
		return strings.Join(own[:n-1], ", ") + " and " + own[n-1] + " on their own model"
	}
}

// OpenModelRoles lists the roles and the model each runs on. Enter opens a
// role's model list over it, and the list rebuilds when that one closes.
func (h *Host) OpenModelRoles() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	build := func() ([]any, [][]Segment) {
		p, _ := h.resolveProfile("")
		var items []any
		for i, id := range h.roleIDs() {
			r := roles[i]
			item := map[string]any{"text": r.Name, "desc": cmp.Or(id, "no model") + " · " + r.Desc, "verb": "open", "run": "model " + r.Name}
			switch {
			case i == 0 && h.ProfileModelErr() != nil:
				item["key"] = "profile " + p.Name
			case i > 0 && h.Settings.Str("models."+r.Name) == "":
				item["key"] = "follows chat"
			}
			items = append(items, item)
		}
		return items, nil
	}
	items, _ := build()
	ui.OpenOverlay(Overlay{ID: "model roles", Title: "Model roles", Origin: lua.OriginBuiltin, Items: items, Anchor: OverlayBottomRight, Verb: "open", Reload: build})
	return nil
}

// modelDesc is the picker's second column: display name, context window
// and USD per million tokens in/out, each left out when unknown.
func modelDesc(id string, m agent.Model) string {
	var parts []string
	if m.Display != "" && m.Display != id && m.Display != m.ID {
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

// SetModel runs /model's arguments from any goroutine: none opens the chat
// list, "roles" the role list, "update" fetches the catalog, a role its
// list, an id sets chat, and a role with an id, or "chat" to follow it,
// sets that role. The config watches it fires run later on the interpreter
// queue. warn toasts an id the provider does not list; --health leaves
// that to its model row.
func (h *Host) SetModel(args string, warn bool) error {
	f := strings.Fields(args)
	switch {
	case len(f) == 0:
		return h.OpenModelPicker("")
	case len(f) == 1 && f[0] == "roles":
		return h.OpenModelRoles()
	case len(f) == 1 && f[0] == "update":
		return h.updateCatalog()
	case len(f) == 1 && isRole(f[0]):
		return h.OpenModelPicker(f[0])
	case len(f) == 1:
		f = []string{"chat", f[0]}
	case len(f) > 2 || !isRole(f[0]):
		return Usagef("usage: /model [roles | update | <role> | [<role>] provider/model | <role> chat]; roles: %s", strings.Join(roleNames(), ", "))
	}
	role, id := f[0], f[1]
	if id == "chat" { // chat following chat is the default model
		id = ""
	}
	if err := h.modelSetErr(role, id); err != nil {
		return err
	}
	if err := h.setWatched("models."+role, id); err != nil {
		return err
	}
	if ui, err := h.Screen(); err == nil { // a /model under the roles list shows the pick when it comes back
		o := h.modelPicker("")
		o.Refresh = true
		ui.OpenOverlay(o)
	}
	if warn && id != "" {
		h.warnUnlisted(id)
	}
	return nil
}

// updateCatalog fetches the catwalk catalog for this and later boots. With
// a UI it runs as a busy job and reloads config so the registry takes it,
// even on a 304: another zemi may have saved the file this one never ran.
// Headless it only saves, before the run exits, and says so on stderr.
func (h *Host) updateCatalog() error {
	kept := "bundled"
	if !h.Builtin.Load().Info.Fetched.IsZero() {
		kept = "saved"
	}
	headless := h.hooks() == nil
	run := func(ctx context.Context) error {
		c, changed, err := agent.UpdateCatalog(ctx, agent.CatalogURL, h.Dirs.DataHome, os.Getenv)
		if err != nil {
			return fmt.Errorf("%w; the %s one stays", err, kept)
		}
		h.Builtin.Store(&c)
		title, body := "Model catalog is up to date", ""
		if changed {
			title, body = "Model catalog updated", fmt.Sprintf("%d %s, %d %s", c.Info.Providers, plural(c.Info.Providers, "provider", "providers"),
				c.Info.Models, plural(c.Info.Models, "model", "models"))
		}
		if !headless {
			h.Notify(logging.Info, title, body)
		} else if body != "" {
			fmt.Fprintf(h.Stderr, "zemi: %s: %s\n", title, body)
		} else {
			fmt.Fprintf(h.Stderr, "zemi: %s\n", title)
		}
		return nil
	}
	if headless {
		// an interrupt ends -p with 130 (printTurn), which this fetch must not outlast
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		defer stop()
		if err := run(ctx); err != nil {
			return fmt.Errorf("could not update the model catalog: %w", err)
		}
		return nil
	}
	h.GoSafe("model update", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := func() error {
			defer h.Busy("Updating the model catalog", cancel)()
			return run(ctx)
		}()
		switch {
		case err == nil:
			_ = h.ReloadConfig()
		case ctx.Err() == nil: // not esc
			h.Notify(logging.Error, "Could not update the model catalog", err.Error())
		}
	})
	return nil
}

// CompleteModel offers /model's arguments in SetModel's grammar.
func (h *Host) CompleteModel(lead, line string) []string {
	first := []string{"roles", "update"}
	var ids []string
	for _, r := range roles {
		first = append(first, r.Name)
	}
	for _, s := range h.Providers.List() {
		for _, m := range s.Models {
			ids = append(ids, s.ID+"/"+m.ID)
		}
	}
	return subcommandCompleter(append(first, ids...), func(_ *Host, sub string) []string {
		switch {
		case sub == "chat":
			return ids
		case isRole(sub):
			return append([]string{"chat"}, ids...)
		}
		return nil
	})(h, lead, line)
}

const unlistedTitle = "Model not listed"

// warnUnlisted toasts a pick the provider does not list. It reads the pick,
// not the model row: mid-turn the row is the running turn's model.
func (h *Host) warnUnlisted(id string) {
	if _, ok := h.modelSpec(id); !ok {
		h.Notify(logging.Warn, unlistedTitle, unlistedDetail(id))
	}
}

// modelSetErr refuses a model pick under a provider nobody registered, or
// a chat pick the active profile would hide. An id the provider does not
// list is taken and warned about, like -m and config: the provider decides.
func (h *Host) modelSetErr(role, id string) error {
	if err := h.ProfileModelErr(); err != nil && role == "chat" {
		return err
	}
	prov, _, _ := strings.Cut(id, "/")
	if _, ok := h.Providers.Lookup(prov); !ok && id != "" {
		return usageError{h.modelErr(id)}
	}
	return nil
}

// WarnModel toasts the model row's warning after a reload, if any: a
// config the provider does not list would otherwise run silently until the
// provider refuses it.
func (h *Host) WarnModel() {
	if row := h.modelRow(); row.level != HealthOK {
		h.Notify(logging.Warn, map[HealthLevel]string{HealthError: "Model unavailable", HealthWarn: unlistedTitle}[row.level], row.detail)
	}
}

// OfferModel opens the model picker when no model can run the next send,
// so a first run starts on the keys to export rather than a failed send. A
// failed init.lua is its own news: the providers it adds never ran.
func (h *Host) OfferModel() {
	if h.InitError() == "" && h.modelRow().level == HealthError {
		_ = h.OpenModelPicker("")
	}
}

// ProfileModelErr refuses a models.chat write the active profile would
// hide: the key would change and the conversation would not.
func (h *Host) ProfileModelErr() error {
	if p, _ := h.resolveProfile(""); p != nil && p.Model != "" && p.Model != "chat" {
		return Usagef("profile %s sets its model; /profile default or drop model from it", p.Name)
	}
	return nil
}

// setWatched writes a config key from any goroutine: the watches it fires
// run later on the interpreter queue. It checks only the kind: SetConfig
// sends a key with a setter of its own there first.
func (h *Host) setWatched(path string, val any) error {
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
	names := []string{"default", "planner"}
	for _, n := range slices.Sorted(maps.Keys(h.agents.profiles)) {
		if n != "default" && n != "planner" {
			names = append(names, n)
		}
	}
	return names
}

// SetProfile switches the profile the next send runs as (/profile <name>).
// Picking a read-only agent remembers the one it replaced, so approving a
// plan can return to it (approvePlan).
func (h *Host) SetProfile(name string) error {
	name = cmp.Or(name, "default") // resolveProfile reads "" as the active one
	if _, err := h.resolveProfile(name); err != nil {
		return Usagef("unknown profile %q (want %s)", name, strings.Join(h.ProfileNames(), ", "))
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
	i := max(slices.Index(names, cur), 0)
	return h.SetProfile(names[((i+step)%n+n)%n])
}

// implementNow is the message a fresh send carries to the restored agent after
// its plan is approved; the plan itself stays in history.
var implementNow = agent.Note("The user approved the plan above. " + tools.ImplementPlan)

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
var planNudge = agent.Note(planNudgeText)

const planNudgeText = "Your reply ended in plain text, which the user cannot approve or answer. If it asked something, ask it with the question tool; if it was a plan, present it with the plan tool; if it answered a question, do not repeat the answer: ask with the question tool what to plan next."

// nudgesPlan reports whether a turn run as p is nudged for ending in plain
// text: a read-only agent with the plan tool. Headless drops the tool and
// planHeadless asks for the plan as the reply, so there is nothing to
// present it with.
func (h *Host) nudgesPlan(p *Profile) bool {
	return p != nil && p.ReadOnly && hasTool(h.profileTools(p), tools.ToolPlan)
}

// planFollowup runs after a turn goroutine ends. An approved plan left for
// a cancelled turn (nextTurn hands off an ended one) is sent to the restored
// agent to implement; otherwise a turn nudgesPlan held for that ended in
// plain text is nudged once per user message. Runs on the turn goroutine,
// never the UI one.
func (h *Host) planFollowup(runner *agent.Runner, nudge bool) {
	if msg := h.takePlanApproved(); msg != "" {
		h.startImplementation(msg)
		return
	}
	if !nudge || plainTextEnd(runner) == "" {
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
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	cur, sel := h.Settings.Str("agent.profile"), 0
	var items []any
	for i, name := range h.ProfileNames() {
		item := map[string]any{"text": name, "run": "profile " + name, "desc": "the shipped prompt and every tool"}
		h.agents.mu.Lock()
		_, defined := h.agents.profiles[name]
		h.agents.mu.Unlock()
		if name == "planner" && !defined {
			item["desc"] = "read-only: plan and present for approval"
		} else if p, _ := h.resolveProfile(name); p != nil {
			item["desc"] = profileDesc(p)
		}
		if name == cur {
			item["key"], sel = "current", i
		}
		items = append(items, item)
	}
	ui.OpenOverlay(Overlay{ID: "profile", Title: "Profile", Origin: lua.OriginBuiltin, Items: items, Selected: sel, Anchor: OverlayBottomRight, Verb: "pick"})
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
		return p.Model + " · " + tools
	}
	return tools
}

func (h *Host) agentNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	profile := L.NewTable()
	tb.RawSetString("profile", profile)
	profile.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		spec, ok := L.Get(2).(*glua.LTable)
		if !ok {
			L.RaiseError("zemi.agent.profile.add: spec must be a table")
			return 0
		}
		h.refuseBelowUser(L, "agent.profile.add")
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
	// A plugin's text goes to the model behind agent.Attached, so it is never
	// the user's words, and it marks the session not local-only (DESIGN §6.3).
	tb.RawSetString("send", L.NewFunction(func(L *glua.LState) int {
		text := L.CheckString(1)
		plugin := ""
		if h.RT.CurrentOrigin(L) == lua.OriginPlugin {
			if h.hooks() == nil {
				// nothing bounds a chain of turns, and -p exits after its own
				L.RaiseError("zemi.agent.send: a plugin cannot send in a print run (-p)")
			}
			h.luaGate(L, "lua.agent.send", "*")
			plugin = h.principal(L)
			text = agent.Attached(plugin, text)
		} else {
			h.refuseBelowUser(L, "agent.send")
		}
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
			if plugin != "" {
				h.markNotLocal(h.AgentSessionID(), plugin) // before the turn could write a memory
			}
			err := h.AgentSend(text)
			switch {
			case err != nil:
				h.Log.Warn("agent", "zemi.agent.send failed", map[string]any{"error": err.Error()})
				h.Notify(logging.Error, "Could not send", h.friendlyError(err))
			case plugin != "":
				h.markNotLocal(h.AgentSessionID(), plugin) // a fresh session exists only now
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
				ui.ComposerEdit(ComposerSet, strings.Join(q, "\n"), -1)
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
		// user config only; every other origin may only add deny/ask rules,
		// which match every principal, so none can aim at another plugin.
		if origin := h.RT.CurrentOrigin(L); origin != lua.OriginUser {
			if r.Decision == perm.Allow {
				L.RaiseError("zemi.permission.rule.add: allow rules may only come from user config")
				return 0
			}
			if rawString(spec, "principal") != "" {
				L.RaiseError("zemi.permission.rule.add: only user config scopes a rule to a principal")
				return 0
			}
			r.Source = string(origin)
		}
		if p := rawString(spec, "principal"); p != "" {
			if name, ok := perm.CutPluginPrincipal(p); !ok || name == "" {
				L.RaiseError(`zemi.permission.rule.add: principal must be "plugin:<name>", got %q`, p)
				return 0
			}
			r.Principal = p
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
			if r.Principal != "" {
				row.RawSetString("principal", glua.LString(r.Principal))
			}
			out.Append(row)
		}
		L.Push(out)
		return 1
	}))
	tb.RawSetString("rule", rule)
	tb.RawSetString("decision", enumTable(L, perm.Allow, perm.Ask, perm.Deny))
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
	tb.RawSetString("rename", L.NewFunction(func(L *glua.LState) int {
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

// readLog is the live session's record of the files the model read. It
// outlives the pipeline, which is rebuilt every send, so a file read in one
// turn can be edited in the next. Only the live session is kept: an older
// one cannot be edited, and holding every path of every session would grow
// without end.
func (h *Host) readLog() *sync.Map { reads, _ := h.sessionLogs(); return reads }

// sessionLogs is the live session's read log and its loaded MCP tools.
func (h *Host) sessionLogs() (reads, loaded *sync.Map) {
	sid := h.AgentSessionID()
	ag := h.agents
	ag.readMu.Lock()
	defer ag.readMu.Unlock()
	if ag.reads[sid] == nil {
		ag.reads = map[string]*sync.Map{sid: new(sync.Map)}
		ag.instr = map[string]bool{}
		ag.loaded = new(sync.Map)
	}
	return ag.reads[sid], ag.loaded
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
			if !ok || !slices.Contains([]string{tools.ToolRead, tools.ToolWrite, tools.ToolEdit}, call.ToolName) {
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
		Post:      h.toolPost,
		Principal: perm.PrincipalModel,
		Jobs:      h.jobs,
		Skills:    h.skillIndex,
		Images: func() bool {
			h.agents.mu.Lock()
			id := h.liveModelID()
			h.agents.mu.Unlock()
			return h.takesImages(id)
		},
		Remember: func(scope store.Scope, text, quote, replaces string) (string, error) {
			return h.memoryTool(h.AgentSessionID(), scope, text, quote, replaces)
		},
		Scratch: h.inScratch,
		Protected: func(p string) string {
			if h.inScratch(p) {
				return ""
			}
			return h.protectedWhy(p)
		},
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
