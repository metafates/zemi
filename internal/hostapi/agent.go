// agent.go is the agent core in the host (DESIGN §6.3): the
// provider/model/agent/permission/session namespaces, turn streaming into the
// UI, message persistence and the steering queue.

package hostapi

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/set"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// PermRequest is one interactive permission ask for the overlay.
type PermRequest struct {
	Tool    string // the asking tool (shell, edit, mcp_github_search)
	Subject string
	Arg     string
	Reason  string
	Preview string // a file tool's unified diff, a script, a tool call's input
	// Description is the model's summary of a command, not checked against it
	Description string
	// Agent is the subagent asking, as the prompt names it ("explore
	// agent"), "" for the main agent
	Agent string
	// Owner is whose code a Lua tool runs, as an overlay badge names it
	// ("plugin:jira"), "" for the user's and zemi's own
	Owner string
	// Script names the lua tool script a nested call came from, as the badge
	// says it ("lua script"), "" for a call the model made itself
	Script string
}

// StreamTool is one tool call in the live stream view (DESIGN §6.3).
type StreamTool struct {
	ID       string
	Label    string // the model's short description of a command
	Started  time.Time
	Detail   string // live progress line (subagents, workflows, a shell command's last output line)
	Tail     string // a running shell call's output tail, what its view reads
	Run      string // agent tool: the run id, so the row can open its transcript
	Diff     string // file tools: the change as "- "/"+ " lines
	Output   string // the last lines of the result (shortOutput)
	Name     string
	Input    string
	Streamed int    // input bytes streamed while the call is pending, what a write's row shows
	Raw      string // the call's raw args JSON: the log's tool row holds it, where the digest cannot rebuild the label
	Status   CallStatus
	Job      string        // shell and job_start calls: the job the work went to
	Out      tools.Outcome // what the tool reported beyond its text, read when it ends
	logged   bool          // the pair reached the log at its step's StepDone; the cut path must not log it twice
	partial  string        // the head of the streaming input, until Label is known
}

// agentHost owns the live chat: the model slots config registered, the
// runner, the steering queue and the stream view the UI renders.
type agentHost struct {
	mu     sync.Mutex
	runner *agent.Runner
	// outcomes is what each running call's tool reported beyond its text,
	// keyed by call id and read once the call ends
	outcomes map[string]tools.Outcome
	// rows is what each running call's tool reported beyond its text, keyed
	// by call id and stored with the message that drew it
	rows map[string]rowMeta

	// targetsMu guards targets, the slots zemi.model.add registered. It is a
	// leaf: roleModel reads it with mu held (liveModelID), so nothing under it
	// takes another lock.
	targetsMu sync.RWMutex
	targets   map[string]*modelTarget

	active    bool
	textBuf   string
	reasonBuf string
	// stretchMS holds the wall time of each reasoning stretch the turn has
	// flushed since the step began, oldest first: the step's message persists
	// its reasoning parts in the same order, and each takes the next time.
	stretchMS []time.Duration
	// the stream state and transcript length when the current step began: a
	// provider retry streams the step again from there
	stepText, stepReason string
	stepTools            []StreamTool
	stepRows             int
	stepThought          time.Duration
	stepTurnThought      time.Duration
	stepSettled          bool // the step's OnStepFinish ran, so its calls are logged
	tools                []StreamTool
	pendingWork          []workNote // work rows parked behind an unsettled step
	workHeld             bool       // a work row parked: the next ones follow it
	unflushed            bool       // rows the log took that no flush has drawn yet
	steering             []string
	steeredTurn          bool   // the running turn's input came from the steering queue
	task                 string // the running turn's own input, one line: the program status report names it
	// spoke is closed when the user's own message arrives during a turn
	// (UserSpoke): a foreground call waiting on a long run hands it off
	// instead of holding the turn past the message. Re-armed where the queue
	// is read, so a spent signal never detaches a later call.
	spoke     chan struct{}
	modelSeed string
	turnModel string // model id the runner was built with, "" for a bare one: its price, and compact's cached prefix

	// session usage (DESIGN §8 statusline tally): sessIn/sessOut accumulate
	// every turn's counts, lastIn is the context the latest step used
	// (agent.ContextUsed, the current context estimate). ctxWin: seeded
	// override (tests); 0 resolves via contextWindow.
	sessIn  int64
	sessOut int64
	// tallyLoaded: the store row seeded the fields above at the first read
	tallyLoaded bool
	// cache reads and the input billed with them, for the /usage share
	cacheRead   int64
	cacheBilled int64
	lastIn      int64
	ctxWin      int64
	sessCost    float64 // USD, from model spec prices

	// readMu guards reads, instr and loaded, the per-session bookkeeping
	// the tools carry (what the model has read, which instruction files it
	// has been given, which MCP tools tool_search loaded).
	readMu sync.Mutex
	reads  map[string]*sync.Map
	instr  set.Set[string]
	loaded *sync.Map

	// promptTokens estimates the system prompt and tool schemas the runner
	// carries; every prompt holds them, so the context estimate counts them.
	// promptParts is the same total broken down for /usage.
	promptTokens int64
	promptParts  []promptPart
	// pin is the main agent's fixed prefix (DESIGN §6.3), sent verbatim
	// until its key moves or a drop point clears it; pinDrops counts drops,
	// and pinMu holds one build at a time, since two builds would both run
	// the assemble handlers
	pin      promptPin
	pinDrops int
	pinMu    sync.Mutex

	// turn timing: turnStart pins the clock at AgentSend, thinking
	// tracks the open reasoning block, thought accumulates closed ones
	// since the last flush, turnThought the turn's whole total (the stat
	// the log's reply row rides) and stepThinking those of the current step.
	// clock is the host's, which the live snapshot reads: a stamp on one
	// clock and a read on another measures the gap between them.
	clock        lua.Clock
	turnStart    time.Time
	thinkStart   time.Time
	thinking     bool
	thought      time.Duration
	turnThought  time.Duration
	stepThinking time.Duration
	// promptWait sums the time turns spent blocked on the user at a prompt
	// (ask, plan, question): the reply's stat bills the model, not the reader
	promptWait time.Duration

	// retryAt is when the next provider retry fires, zero again once the
	// retried call streams; retries counts them this turn (the working row
	// counts down instead of looking stuck). turns counts the turns begun
	// (beginTurn), which resets retries, so a reader tells one turn's count
	// from the last one's by it.
	retryAt  time.Time
	retries  int
	retryWhy string // friendlyError of the failure being retried
	turns    int

	holdImages   bool // a 400 over image parts is resent without them, not reported
	holdCap      bool // a refused output cap is resent without it, not reported
	holdOverflow bool // a too-large context is merged and resent once, not reported

	// hookCtx is what the plugins' SessionStart hooks captured for the chat.
	hookCtx string

	// shellNotes holds shell command output waiting for the next message.
	shellNotes []string

	// todoNag nudges a stale todo list: the items it holds, how many steps
	// have run since the last write, and whether a remind is already queued.
	// Any todos write resets it, so an update (or a tick) stops it.
	todoList    []store.Todo // the list as the last todos write left it
	todoSteps   int          // main-runner steps since the last todos write
	todoNagAt   int          // the step count the last nag fired at
	todoTouched bool         // a todos call ran this step
	todoPending bool
	// todoNag waits for the next step boundary of the running turn, outside
	// steering: a turn that ends first drops it instead of starting another.
	todoNag string

	// bgNoted holds the runs an end-of-turn note already named, so a model
	// that ends its turn again anyway is not nagged twice about one fleet.
	bgNoted map[string]bool
	// bgNoteTurn marks the running turn as one the note started, so a model
	// that ignores it cannot be driven in circles by notes of its own.
	bgNoteTurn bool

	// goal is the session's goal (/goal), nil for none; goalPending the
	// condition of a /goal sent and not yet stored; goalRuns counts the turns
	// it started in a row.
	goal        *Goal
	goalPending string
	goalRuns    int
	// loop is the session's loop (/loop), nil for none; loopStop stops its
	// armed timer, loopRun is the loop whose run was sent last, and byLoop
	// says the running or last chain of turns began with a loop run.
	// loopEnded holds the why of the stop that cleared loop, for the UI's
	// end-of-loop alert.
	loop      *Loop
	loopStop  func() bool
	loopRun   *Loop
	loopEnded string
	byLoop    bool

	env string // pinned environment section (environmentPrompt)
}

func newAgentHost(clock lua.Clock) *agentHost {
	return &agentHost{targets: map[string]*modelTarget{}, clock: clock}
}

// host wiring

// Effort is the reasoning level modelID runs at under the agent.effort pick.
func (h *Host) Effort(modelID string) string {
	return h.Providers.Effort(agent.ModelID(modelID), agent.Effort(h.Settings.Str(config.KeyAgentEffort)))
}

// liveModelID is the model turns run on: the running turn's, else the
// conversation's default pick (ag.mu held).
func (h *Host) liveModelID() string {
	ag := h.agents
	if ag.active && ag.turnModel != "" {
		return ag.turnModel
	}
	return h.roleModel(_defaultRole)
}

// sessionModelID is liveModelID for callers not holding ag.mu.
func (h *Host) sessionModelID() string {
	h.agents.mu.Lock()
	defer h.agents.mu.Unlock()
	return h.liveModelID()
}

func (h *Host) builtinTools(pipe *tools.Pipeline) []fantasy.AgentTool {
	return append(tools.All(pipe), h.agentTool(pipe), h.workflowTool(pipe), h.luaEvalTool(pipe), h.zoomTool(), h.dateTool())
}

// allTools is every tool a run could get, in one order: the builtins, the
// Lua tools, then the MCP ones. A caller filters out what its run may not
// have.
func (h *Host) allTools(pipe *tools.Pipeline) []fantasy.AgentTool {
	return append(append(h.builtinTools(pipe), h.luaFantasyTools(pipe)...), h.mcpFantasyTools(pipe)...)
}

// _headlessExcluded needs a user at a terminal: with no UI hooks these tools
// can only refuse, so a no-UI boot does not pay for their schemas every step.
var _headlessExcluded = set.Of(tools.ToolQuestion, tools.ToolPlan)

// toolBuilder builds the run's tool set for a given pipeline: builtin, Lua and
// MCP tools. The lua tool builds its scripts' calls through it, so a script
// reaches exactly what the run has.
func (h *Host) toolBuilder(skip set.Set[tools.ToolName]) func(*tools.Pipeline) ([]fantasy.AgentTool, []string) {
	return func(pipe *tools.Pipeline) ([]fantasy.AgentTool, []string) {
		return filterTools(h.allTools(pipe), nil, skip)
	}
}

// mainTools selects the conversation's tool set over a pipeline of its own.
func (h *Host) mainTools() []fantasy.AgentTool {
	pipe := h.PipelineFor()
	pipe.Images = func() bool { return h.takesImages(h.roleModel(_defaultRole)) }
	pipe.Audio = func() bool { return h.takesAudio(h.roleModel(_defaultRole)) }
	return h.toolsFor(pipe)
}

// toolsFor builds one run's tool set over a pipeline it owns, whose Images says
// which model the set is built for.
func (h *Host) toolsFor(pipe *tools.Pipeline) []fantasy.AgentTool {
	var skip set.Set[tools.ToolName]
	if h.hooks() == nil {
		skip = _headlessExcluded
	}
	build := h.toolBuilder(skip)
	out, _ := build(pipe)
	wireLua(out, build)
	return out
}

// wireLua gives each lua tool the builder its script's calls come from, so a
// script reaches exactly the set the model has.
func wireLua(out []fantasy.AgentTool, build func(*tools.Pipeline) ([]fantasy.AgentTool, []string)) {
	for _, t := range out {
		if lt, ok := t.(*luaEvalTool); ok {
			lt.build = build
		}
	}
}

func (h *Host) ensureRunner(toolset []fantasy.AgentTool) (*agent.Runner, error) {
	id := h.roleModel(_defaultRole)
	model, err := h.resolveTurnModel(id)
	if err != nil {
		return nil, err
	}
	_, loaded := h.sessionLogs()
	toolset, active := h.deferMCP(toolset, loaded)
	toolset, sys, ctxMsg, parts := h.pinPrompt(toolset)
	r := agent.NewRunner(model, toolset, sys)
	r.Context, r.ActiveTools = ctxMsg, active
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
	h.agents.promptParts, h.agents.promptTokens = sizes, int64(chars/4)
	h.agents.mu.Unlock()
	sid := store.MainSessionID
	r.ProviderOptions = h.Providers.CallOptions(agent.ModelID(id), h.Effort(id), sid)
	r.Headers = h.Providers.SessionHeaders(agent.ModelID(id), sid)
	r.MaxOutputTokens = h.maxOutput(id)
	r.Hooks = h.streamHooks()
	r.Persist = h.persistMessage
	r.Steer = h.deliverSteering
	r.TextOnly = !h.takesImages(id)
	r.Compose = func(text string) fantasy.Message { return h.composeUserMessage(text, !r.TextOnly) }
	return r, nil
}

// promptPin is the prefix one key was built for: the system text, its
// sections and each tool's Info, and the instruction file versions messages
// carried since (instructionEdits).
type promptPin struct {
	key, core, text, context string
	parts                    promptParts
	infos                    map[string]fantasy.ToolInfo
	sent                     map[string]time.Time
}

// dropPin makes the next send build the prompt again. The text stays for
// /prompt and for the rest of a running turn, which still sends it. ag.mu held.
func (ag *agentHost) dropPin() { ag.pin.key, ag.pinDrops = "", ag.pinDrops+1 }

// _contextRole marks a run's stored context message; no transcript reader
// looks at the role.
const _contextRole = "context"

// pinnedTool sends the Info the pin was built with, so a description read
// from files (agent definitions, saved workflows) cannot move the prefix.
type pinnedTool struct {
	fantasy.AgentTool

	info fantasy.ToolInfo
}

func (t pinnedTool) Info() fantasy.ToolInfo { return t.info }

// pinPrompt gives the pinned prompt while its key holds and builds and pins a
// new one otherwise. The key is what the user switches (model, trust),
// whether the model takes images (read's
// description says) and each tool's name and input schema, since a stale
// schema fails calls. Descriptions, instruction files, skills and
// agent.assemble output wait for a drop point (deleteRecipe). A reload drops
// nothing: the config watcher reloads on any config-tree write, the model's
// own skill writes included.
func (h *Host) pinPrompt(toolset []fantasy.AgentTool) (out []fantasy.AgentTool, system, ctxMsg string, parts promptParts) {
	id := h.roleModel(_defaultRole)
	trusted, images := strconv.FormatBool(h.ProjectTrusted()), strconv.FormatBool(h.takesImages(id))
	key, core := []string{id, trusted, images}, []string{id, trusted, images}
	infos := make([]fantasy.ToolInfo, len(toolset))
	for i, t := range toolset {
		infos[i] = t.Info()
		// Parallel is in the key: fantasy reads it per step, not the provider
		schema, _ := json.Marshal([]any{infos[i].Parameters, infos[i].Required, infos[i].Parallel}, json.Deterministic(true))
		pair := []string{infos[i].Name, string(schema)}
		key = append(key, pair...)
		// the MCP tools move on their own (a server connecting, a tools/list
		// refresh), which is not a switch: core is key without them
		if !strings.HasPrefix(infos[i].Name, _mcpToolPrefix) && infos[i].Name != string(tools.ToolSearch) {
			core = append(core, pair...)
		}
	}
	k := strings.Join(key, "\x00")
	ag := h.agents
	ag.pinMu.Lock()
	defer ag.pinMu.Unlock()
	ag.mu.Lock()
	pin, drops := ag.pin, ag.pinDrops
	ag.mu.Unlock()
	if pin.key != k {
		pin = promptPin{key: k, core: strings.Join(core, "\x00"), infos: map[string]fantasy.ToolInfo{}}
		pin.text, pin.context, pin.parts = h.buildSystemParts(toolset)
		for _, info := range infos {
			pin.infos[info.Name] = info
		}
		ag.mu.Lock()
		// a drop during the build means it may have read what the drop replaced
		if ag.pinDrops == drops {
			ag.pin = pin
		}
		ag.mu.Unlock()
	}
	out = make([]fantasy.AgentTool, len(toolset))
	for i, t := range toolset {
		out[i] = pinnedTool{AgentTool: t, info: pin.infos[infos[i].Name]}
	}
	return out, pin.text, pin.context, pin.parts
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
		schema, _ := json.Marshal(info.Parameters, json.Deterministic(true))
		n := len(info.Name) + len(info.Description) + len(schema)
		if strings.HasPrefix(info.Name, _mcpToolPrefix) || info.Name == string(tools.ToolSearch) {
			mcp += n
		} else {
			builtin += n
		}
	}
	return []promptPart{
		{"system", p.base},
		{"tools", builtin},
		{"mcp", mcp},
		{"instructions", p.instructions},
		{"lua", p.lua},
		{"skills", p.skills},
	}
}

// SystemPrompt is the pinned system prompt and context message the last send
// carried, "" before the first one. /usage measures it; this is the text, so
// an assemble handler or an instruction file the budget cut can be read
// instead of guessed at. It is never rebuilt here: assembling it runs Lua
// handlers on the interpreter, which a Lua caller would deadlock on.
func (h *Host) SystemPrompt() string {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if ag.pin.context == "" {
		return ag.pin.text
	}
	return ag.pin.text + "\n\n" + ag.pin.context
}

// promptBreakdown is the fixed prompt the last runner build measured, in
// tokens: what every request pays for before the first message.
func (h *Host) promptBreakdown() ([]promptPart, int64) {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return ag.promptParts, ag.promptTokens
}

// AgentSend sends one message to the agent (DESIGN §4, §6.3). Idle, it starts
// a turn; during a turn it queues, and the queue joins the running turn at its
// next step boundary. Safe from any goroutine.
func (h *Host) AgentSend(text string) error {
	_, err := h.agentSend(text)
	return err
}

// AgentSendUser sends the user's own typed line: AgentSend, and a message that
// joined a running turn is marked (UserSpoke), so a foreground agent or
// workflow call waiting on a long run hands off and the turn reads the message
// at its next step boundary instead of waiting for that run (DESIGN §4). A
// message that starts a turn marks nothing: its own calls wait as usual.
// A literal marker in the user's words is defused: a paste carries it, and it
// must not cut the user's own words out of their message.
func (h *Host) AgentSendUser(text string) error {
	_, err := h.agentSendUser(text)
	return err
}

// agentSendUser is AgentSendUser, reporting whether the message joined a
// running turn.
func (h *Host) agentSendUser(text string) (queued bool, err error) {
	text = agent.Defuse(text)
	if queued, err = h.agentSend(text); queued {
		h.UserSpoke()
	}
	return queued, err
}

// _maxMessageChars caps one sent message's typed text: a paste beyond it
// would blow any model's window and wedge the turn on a refusal instead of
// failing here, where the text is still one paste away.
// NOTE: a fixed ceiling, not the model's own window; a file goes as @path.
const _maxMessageChars = 400_000

// checkMessage refuses what no turn can take: an empty message, or a paste
// beyond the ceiling.
func checkMessage(text string) error {
	if strings.TrimSpace(text) == "" {
		// a provider refuses an empty user message, and where it landed would
		// be the middle of a turn
		return errors.New("nothing to send")
	}
	if utf8.RuneCountInString(text) > _maxMessageChars {
		return fmt.Errorf("the message is over the %dk character limit; save it to a file and attach it with @path", _maxMessageChars/1000)
	}
	return nil
}

// agentSend is AgentSend, reporting whether the message joined a running turn.
func (h *Host) agentSend(text string) (queued bool, err error) {
	if err := checkMessage(text); err != nil {
		return false, err
	}
	ag := h.agents
	ag.mu.Lock()
	if ag.active {
		ag.steering = append(ag.steering, text)
		ag.mu.Unlock()
		h.refresh()
		h.Log.Debug("message steered into the running turn", "origin", "agent",
			"text", clipLine(sanitize.String(text), 120))
		return true, nil
	}
	// Held from here on: building the prompt waits on the interpreter, and a
	// send arriving meanwhile must queue instead of starting a second turn.
	ag.steeredTurn = false // the user's own words open the turn
	scope := h.claim()
	ag.mu.Unlock()
	return false, h.sendClaimed(scope, text)
}

// drainQueue starts a turn for what a background agent's report left queued
// (it arrived under a claim that ran no turn, the plan popup's) once none is
// left and no turn holds the agent.
func (h *Host) drainQueue() {
	ag := h.agents
	ag.mu.Lock()
	if ag.active || len(ag.steering) == 0 || h.tasks.runningFor(store.MainSessionID) > 0 {
		ag.mu.Unlock()
		return
	}
	scope := h.claim()
	ag.mu.Unlock()
	h.handOff(scope)
}

// sendClaimed starts a turn for text on an agent already claimed with scope.
// A panic before the turn goroutine starts is an error, so the caller keeps
// the text and a loop stops.
func (h *Host) sendClaimed(scope *turnScope, text string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			h.clearCrashedTurn(r)
			scope.cancel()
			h.refresh()
			err = errors.New("it crashed; /errors has the cause")
		}
	}()
	ag := h.agents
	// the turn's words read before the runner builds (interpreter work):
	// the UI's first working report names what the turn is on
	ag.mu.Lock()
	ag.task = clipLine(text, 200)
	ag.mu.Unlock()
	fail := func(err error) error {
		h.endScope(scope)
		h.refresh()
		return err
	}
	id := h.roleModel(_defaultRole)
	if _, err := h.resolveTurnModel(id); err != nil {
		return fail(err)
	}
	h.captureSessionStart()
	toolset := h.mainTools()
	runner, err := h.ensureRunner(toolset)
	if err != nil {
		return fail(err)
	}
	ag.mu.Lock()
	ag.runner = runner
	ag.modelSeed = ""
	ag.turnModel = id
	if ag.byLoop = strings.Contains(text, _loopTag); !ag.byLoop {
		ag.goalRuns = 0 // someone started it: neither the goal nor a loop
	}
	ag.beginTurn()
	ag.mu.Unlock()
	h.startTurn(text)
	h.Log.Info("send", "origin", "agent", "session", h.AgentSessionID(),
		"text", clipLine(sanitize.String(text), 120))

	h.GoSafe("agent turn", func() {
		h.runTurns(runner, scope, text)
	})
	return nil
}

// claim holds the agent with a fresh turn scope (ag.mu held). The scope is
// stored with the claim, so a cancel arriving before the turn goroutine runs
// is kept.
func (h *Host) claim() *turnScope {
	ctx, cancel := context.WithCancel(context.Background())
	t := &turnScope{ctx: ctx, cancel: cancel}
	h.agents.active = true
	h.agents.spoke = nil // a new turn: the last one's spoken signal is spent
	h.turn.Store(t)
	return t
}

// userSpoke is the channel a foreground call waits on for the user's own
// message, created on first use so a call started outside a turn hears it too.
func (h *Host) userSpoke() <-chan struct{} {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return ag.spokeLocked()
}

// spokeLocked is the signal in force, a fresh open one when none is (ag.mu held).
func (ag *agentHost) spokeLocked() chan struct{} {
	if ag.spoke == nil {
		ag.spoke = make(chan struct{})
	}
	return ag.spoke
}

// UserSpoke marks the user's own typed line: a foreground agent or workflow
// call waiting on a long run hands it off, so the turn reads the message
// instead of waiting for the run. Every waiting call hears it, since the result
// of one call alone cannot release the turn. Host.AgentSendUser raises it, and
// only for a message that joined a running turn; a report, a plugin send or a
// loop never does.
func (h *Host) UserSpoke() {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	ag.spokeClose()
	ag.bgNoted = nil // a user exchange re-arms the hold-the-turn reminder
}

// spokeClose raises this turn's signal (ag.mu held), so a call that waits on it
// later hears it too; one already closed is left alone, since the signal is
// spent for the turn.
func (ag *agentHost) spokeClose() {
	spoke := ag.spokeLocked()
	select {
	case <-spoke: // this turn's calls already heard it
	default:
		close(spoke)
	}
}

// notKept reports a store write a turn made that failed. A session deleted
// under this window repeats on every send, so it is named and recorded once;
// anything else keeps its raw error.
func (h *Host) notKept(what string, err error) {
	if errors.Is(err, store.ErrSessionGone) {
		h.RecordErrorOnce("agent", "", what+": "+h.friendlyError(err))
		return
	}
	h.Log.Warn(what, "origin", "agent", "error", err)
}

// release lets the agent go idle (ag.mu held).
func (h *Host) release() {
	h.agents.active = false
	h.agents.task = "" // the note a sender reads names a live turn, not the last one
	h.turn.Store(nil)
	// on its own goroutine: it runs under ag.mu, which no store call may wait
	// on while it holds the one connection
	h.GoSafe("settle todos", h.settleTodos)
}

// settleTodos closes the list the claim leaves behind: in_progress means a
// turn is doing that item, so with the agent idle it goes back to pending,
// where the next turn's write picks it up. The model's list is kept as it
// wrote it otherwise.
func (h *Host) settleTodos() {
	sid := h.AgentSessionID()
	if h.DB == nil || sid == "" {
		return
	}
	ag := h.agents
	todos, err := h.DB.Todos(sid)
	if err != nil {
		h.Log.Warn("todo list not read", "origin", "agent", "error", err)
		return
	}
	if !slices.ContainsFunc(todos, func(td store.Todo) bool { return td.Status == store.TodoInProgress }) {
		return
	}
	for i := range todos {
		if todos[i].Status == store.TodoInProgress {
			todos[i].Status = store.TodoPending
		}
	}
	// a send may have claimed the agent while this read was in flight, and
	// its turn writes a list of its own: that one is not a leftover to settle.
	ag.mu.Lock()
	stale := ag.active
	ag.mu.Unlock()
	if stale {
		return
	}
	if err := h.DB.SetTodos(sid, todos); err != nil {
		h.notKept("todo list not settled", err)
		return
	}
	h.todosStale.Store(true)
	h.resetTodoNag(todos)
	h.refresh()
}

// endScope releases the agent claimed with scope.
func (h *Host) endScope(scope *turnScope) {
	h.agents.mu.Lock()
	h.release()
	h.agents.mu.Unlock()
	scope.cancel()
}

// handOff ends a claim that ran no turn (/reset). Messages queued
// meanwhile start under the same claim, so a submit cannot jump ahead of
// them; after a cancel the queue waits for the UI to hand it back.
func (h *Host) handOff(scope *turnScope) {
	ag := h.agents
	ag.mu.Lock()
	queued := ""
	if scope.ctx.Err() == nil {
		queued = joinQueue(ag.steering)
		ag.steering = nil
		ag.steeredTurn = queued != "" // the queue's words open the turn
	}
	if queued == "" {
		h.release()
	}
	ag.mu.Unlock()
	if queued == "" {
		scope.cancel()
		h.refresh()
		return
	}
	// sending waits on the interpreter, and a reset may be called from Lua
	h.GoSafe("agent send", func() {
		if err := h.sendClaimed(scope, queued); err != nil {
			h.Notify(slog.LevelError, "Could not send", h.friendlyError(err))
		}
	})
}

// runTurns sends text and every follow-up the queue produces, each turn a
// fresh call that opens on the view: nothing carries between turns.
func (h *Host) runTurns(runner *agent.Runner, scope *turnScope, text string) {
	defer h.endTurnAfterPanic()
	for scope != nil {
		if msg, ok := h.viewMessage(); ok {
			runner.SetHistory([]fantasy.Message{msg})
		} else {
			runner.SetHistory(nil)
		}
		h.armTodoTurnNote()
		err := h.sendTurn(scope.ctx, runner, text)
		// interrupted is the turn's own error, not scope.ctx.Err() alone: the
		// runner's stream cancel can return the turn before the scope's
		// cancel lands, and an interrupt that reads as a finished turn
		// leaves a replaced loop running
		h.loopTurnEnded(runner, text, err, scope.ctx.Err() != nil || errors.Is(err, context.Canceled))
		scope, text = h.nextTurn(scope, err)
	}
}

// sendTurn runs one turn: the provider refusals a turn survives on its own
// are retried (a refused output cap is dropped, image parts a model refuses
// become a note), and a context-too-large refusal runs one view merge batch
// and retries once - the view outgrew the model's window.
func (h *Host) sendTurn(ctx context.Context, runner *agent.Runner, text string) error {
	ag := h.agents
	capDropped, imagesDropped, retried := false, false, false
	for {
		ag.mu.Lock()
		ag.holdOverflow = !retried
		ag.holdImages = !imagesDropped && !runner.TextOnly
		ag.holdCap = !capDropped && runner.MaxOutputTokens > 0
		ag.mu.Unlock()
		err := runner.Send(ctx, text)
		if text != "" { // history holds the user message from here on
			text = ""
		}
		// the turn's verdict logs once at TurnDone with model and usage; this
		// is the per-attempt trace a retried refusal would otherwise lose
		if err != nil && !errors.Is(err, agent.ErrTurnActive) {
			h.Log.Debug("turn attempt failed", "origin", "agent", "error", err)
		}
		// a model whose ceiling is lower than the cap zemi asked for: drop
		// the cap, remember it, and send again rather than failing every turn
		if !capDropped && runner.MaxOutputTokens > 0 && maxTokensRefused(err) {
			capDropped = true
			ag.mu.Lock()
			_capRejected.Store(ag.turnModel, true)
			ag.beginTurn()
			ag.mu.Unlock()
			runner.MaxOutputTokens = 0
			continue
		}
		// a model that refuses image parts: replace them with a note that
		// says so, remember it, and send again rather than ending the turn;
		// an image too large leaves the history, and later ones still go
		if !imagesDropped && !runner.TextOnly && imagesRefused(err) {
			imagesDropped = true
			ag.mu.Lock()
			model := ag.turnModel
			ag.beginTurn()
			ag.mu.Unlock()
			if imageTooLarge(err) {
				runner.DropImages("too large to send")
				h.Notify(slog.LevelWarn, "Image left out", "too large for "+model+"; the turn goes on without it")
			} else {
				runner.TextOnly = true
				_imagesRejected.Store(model, true)
				h.Notify(slog.LevelWarn, "Images left out", model+" takes no images; capabilities = { vision = false } on its model entry skips this")
			}
			continue
		}
		if !retried && contextTooLarge(err) {
			// the held TurnDone kept the turn open; the retry owns it now
			retried = true
			ag.mu.Lock()
			ag.beginTurn()
			ag.mu.Unlock()
			if h.chat != nil {
				h.chat.tree.MergeOnce()
			}
			if msg, ok := h.viewMessage(); ok {
				rest := runner.Snapshot()
				if isViewMessage(rest) {
					rest = rest[1:]
				}
				// the retry keeps the task and the steps the turn finished:
				// only the view message swaps, the refused one carried it
				runner.SetHistory(append([]fantasy.Message{msg}, rest...))
			}
			continue
		}
		// a merge that still leaves the request over the window: with a cap
		// zemi set, the cap is what does not fit - drop it, remember, and send
		// again, as a max_tokens refusal does
		if contextTooLarge(err) && !capDropped && runner.MaxOutputTokens > 0 {
			capDropped = true
			ag.mu.Lock()
			_capRejected.Store(ag.turnModel, true)
			ag.beginTurn()
			ag.mu.Unlock()
			runner.MaxOutputTokens = 0
			continue
		}
		return err
	}
}

func (h *Host) startTurn(text string) {
	h.appendUserRows(text)
	h.agentStream()
	h.EmitAsync(EventAgentStart, map[string]any{"text": text})
}

// appendUserRows shows one message as the model reads it: what was typed.
// The reports and updates behind agent.AttachMarker drew their rows where
// they were delivered (Host.work), never here again.
func (h *Host) appendUserRows(text string) {
	shown, _, _ := userRowsFor(text)
	if shown != "" {
		h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: shown, SentAt: h.now().UnixMilli()})
	}
}

// viewMessage is the fresh turn's opening message: the persisted view as
// one user message of 4-line parts, ok false while the log is empty.
func (h *Host) viewMessage() (fantasy.Message, bool) {
	if h.chat == nil {
		return fantasy.Message{}, false
	}
	return h.chat.viewMessage()
}

// endTurnRunsNote builds the note a cleanly ended turn starts on while work
// that reports to the conversation still runs: the model holds the turn with
// wait all instead of leaving the reports to cold turns on the summary view.
// A workflow's own agents report to the script, never here. A run already
// noted stays silent, so a model that ends the turn anyway is nagged once per
// fleet and the reports still wake it. ag.mu held.
func (h *Host) endTurnRunsNote() string {
	ag := h.agents
	runs := slices.DeleteFunc(h.tasks.runningOf(store.MainSessionID), func(r *agentRun) bool { return r.workflow != "" })
	flows := h.tasks.runningFlows(store.MainSessionID)
	if len(runs) == 0 && len(flows) == 0 {
		ag.bgNoted = nil
		return ""
	}
	var lines []string
	for _, r := range runs {
		if !ag.bgNoted[r.id] {
			lines = append(lines, fmt.Sprintf("agent %s (%s)", r.id, r.label()))
		}
	}
	for _, f := range flows {
		if !ag.bgNoted[f.id] {
			lines = append(lines, fmt.Sprintf("workflow %s (%s)", f.id, f.description))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	ag.bgNoted = map[string]bool{}
	for _, r := range runs {
		ag.bgNoted[r.id] = true
	}
	for _, f := range flows {
		ag.bgNoted[f.id] = true
	}
	return agent.Note(fmt.Sprintf("%s still running. Do not end the turn while background work runs: wait for it (agent wait all wakes you as each one moves), or stop what is no longer needed (agent send_to stop).",
		strings.Join(lines, ", ")))
}

// nextTurn runs after each turn: messages queued after the last step
// boundary become one follow-up turn, otherwise the agent goes idle.
// active stays held across the handoff, so a submit in between queues
// instead of starting a turn out of order. After a
// cancelled turn nothing starts: the queue waits for the UI to hand it back
// to the composer. After a turn that ended cleanly with nothing queued, a
// set goal is checked under the same claim (continueGoal). err is how the
// turn ended.
// _idleResumeWait is how long an interrupt's queue waits before it works
// itself: esc stops the turn, not the reports behind it, and they run once
// the user has been away this long.
const _idleResumeWait = 2 * time.Minute

// armQueueDrain schedules drainQueue, replacing one already waiting.
func (h *Host) armQueueDrain() {
	if h.resumeStop != nil {
		h.resumeStop()
	}
	stop := h.RT.Clock().AfterFunc(_idleResumeWait, func() { h.GoSafe("queue drain", h.drainQueue) })
	h.resumeStop = func() { stop() }
}

func (h *Host) nextTurn(done *turnScope, err error) (*turnScope, string) {
	ag := h.agents
	ag.mu.Lock()
	text := ""
	if done.ctx.Err() != nil {
		// esc stopped the turn; the reports behind it wait for the drain
		// instead of the user's next message
		if len(ag.steering) > 0 {
			h.armQueueDrain()
			h.Notify(slog.LevelInfo, "Queue resumes on its own",
				fmt.Sprintf("%d queued %s run after a couple of minutes; esc interrupts them.",
					len(ag.steering), Plural(len(ag.steering), "message", "messages")))
		}
	} else {
		text = joinQueue(ag.steering)
		ag.steering = nil
		ag.steeredTurn = text != "" // the queue's words open the next turn
	}
	if ag.todoNag != "" {
		// the turn ended without a step boundary to deliver it: the model
		// never saw it, so the tracker restarts rather than nagging at once
		ag.todoNag, ag.todoPending = "", false
		ag.todoSteps, ag.todoNagAt = 0, 0
	}
	// a background run's report starts its own turn, which is checked after
	goal := ag.goal
	if text != "" || err != nil || done.ctx.Err() != nil || h.tasks.runningFor(store.MainSessionID) > 0 {
		goal = nil
	}
	if text != "" {
		ag.goalRuns, ag.byLoop = 0, false
	}
	var next *turnScope
	// the note never chains: a turn it started ends into the reports' own
	// wakes, so a model that ignores it is not driven in circles
	noted := ag.bgNoteTurn
	ag.bgNoteTurn = false
	if text == "" && goal == nil {
		// a clean end with work still running holds the turn instead of
		// releasing it (endTurnRunsNote); an error or an interrupt releases
		if err == nil && done.ctx.Err() == nil && !noted {
			text = h.endTurnRunsNote()
			ag.bgNoteTurn = text != ""
		}
	}
	if text == "" && goal == nil {
		h.release()
	} else {
		ag.beginTurn()
		next = h.claim()
	}
	ag.mu.Unlock()
	done.cancel()
	if goal != nil {
		return h.continueGoal(next, goal)
	}
	if text == "" {
		h.refresh()
		return nil, ""
	}
	h.startTurn(text)
	return next, text
}

// continueGoal checks g under the claim nextTurn kept with next. Its note
// that g does not hold yet is the next turn, unless a message queued during
// the check, which is more current and goes instead; anything else, an
// interrupt included, ends the chain with the goal as the check left it.
func (h *Host) continueGoal(next *turnScope, g *Goal) (*turnScope, string) {
	h.agentStream()
	note := h.checkGoal(next.ctx, g)
	stopped := next.ctx.Err()
	if stopped != nil {
		// before the claim goes, or the UI ends the chain as the last turn ended
		h.streamHooks().TurnDone(stopped, fantasy.Usage{})
	}
	ag := h.agents
	ag.mu.Lock()
	switch {
	case stopped != nil:
		note = ""
	case len(ag.steering) > 0:
		note, ag.steering = joinQueue(ag.steering), nil
		ag.goalRuns, ag.byLoop = 0, false
	case note != "":
		ag.goalRuns++
	}
	if note == "" {
		h.release()
	} else {
		ag.beginTurn()
	}
	ag.mu.Unlock()
	if stopped != nil {
		h.loopInterrupted()
	}
	if note == "" {
		next.cancel()
		h.refresh()
		return nil, ""
	}
	h.startTurn(note)
	return next, note
}

// deliverSteering hands the queue to the running turn at a step boundary.
// The reply streamed so far moves into the transcript first, so each
// steered message shows after the text it interrupted.
func (h *Host) deliverSteering() []string {
	ag := h.agents
	ag.mu.Lock()
	msgs := ag.steering
	steered := len(msgs) > 0
	ag.mu.Unlock()
	// the store is the list's truth: the note counts what is really open. The
	// read stays outside ag.mu, where no store call belongs.
	open := -1
	if steered && h.DB != nil {
		if todos, err := h.DB.Todos(h.AgentSessionID()); err == nil && len(todos) > 0 {
			open = 0
			for _, td := range todos {
				if td.Status != store.TodoDone {
					open++
				}
			}
		}
	}
	ag.mu.Lock()
	msgs = ag.steering
	if len(msgs) > 0 && open >= 0 {
		// new work arrived while the model works: without this it does the
		// work first and adds it to the list afterwards, done already
		note := "Work arrived mid-turn: if it belongs on the todo list, add it with the todos tool."
		if open > 0 {
			note += fmt.Sprintf(" %d %s still open: keep the list current as you take the new work on.", open, Plural(open, "todo is", "todos are"))
		}
		msgs = append(msgs, agent.Note(note))
	}
	if ag.todoNag != "" {
		msgs = append(msgs, ag.todoNag)
	}
	if len(msgs) == 0 {
		ag.mu.Unlock()
		return nil
	}
	ag.steering, ag.todoNag, ag.todoPending = nil, "", false
	ag.spoke = nil // the message is read: a later call is not detached by it
	h.flushOutput(TurnStat{})
	ag.mu.Unlock()
	for _, m := range msgs {
		h.appendUserRows(m)
	}
	return msgs // Runner.Compose expands them
}

// AgentTakeQueue removes and returns the messages still waiting for a step
// boundary, so the UI can hand them back to the composer. What is the model's
// (a report, an update, a plugin send) stays queued for the next turn, and a
// message carrying both keeps its markup and gives back the typed line. While
// a run is shown, its queue is the one waiting: the session's belongs to the
// conversation the view is not showing.
func (h *Host) AgentTakeQueue() []string {
	var q []string
	if run := h.viewedRun(); run != nil {
		run.mu.Lock()
		q, run.queue = takeQueued(run.queue)
		run.mu.Unlock()
	} else {
		ag := h.agents
		ag.mu.Lock()
		q, ag.steering = takeQueued(ag.steering)
		if len(q) > 0 {
			// the user's own lines went back to the composer, so no message
			// reaches the turn: a later call is not detached by a spent signal
			ag.spoke = nil
		}
		ag.mu.Unlock()
	}
	if len(q) > 0 {
		h.refresh()
	}
	return q
}

// takeQueued splits a queue into the messages the user typed, which go back to
// the composer, and the model's own, which stay queued for it.
func takeQueued(msgs []string) (taken, kept []string) {
	for _, m := range msgs {
		shown, attached, tail := userRowsFor(m)
		if attached {
			kept = append(kept, agent.AttachMarker+tail)
		}
		if shown != "" {
			taken = append(taken, shown)
		}
	}
	return taken, kept
}

// shownText is the user's own words ahead of agent.AttachMarker, "" when they
// hold nothing: a message whose text is all the model's, or the whitespace
// joining such messages leaves.
func shownText(head string) string {
	if strings.TrimSpace(head) == "" {
		return ""
	}
	return head
}

// _ownElements open the tail's element right behind the marker: the three the
// queue treats as the model's own words (a report, an update, a plugin's
// send), which takeQueued keeps queued instead of handing back.
var _ownElements = []string{"\n<agent-report ", "\n<agent-update ", "\n<plugin:"}

// userRowsFor splits a user message at agent.AttachMarker into what the user
// typed (shownText's), whether the tail carries the model's own markup, and
// the tail itself (takeQueued re-queues it).
func userRowsFor(text string) (shown string, attached bool, tail string) {
	var head string
	head, tail, _ = strings.Cut(text, agent.AttachMarker)
	for _, e := range _ownElements {
		if strings.HasPrefix(tail, e) {
			return shownText(head), true, tail
		}
	}
	return shownText(head), false, tail
}

// Queued is one message waiting in the steering queue, as the chat draws it:
// what the user typed. The model's own markup draws no row here; its work row
// landed where it was delivered (Host.work).
type Queued struct {
	Text string
}

// queuedRows is what the chat draws of a queue, in the order the model reads
// it: the user's own words, which wait for the next step boundary. The
// model's own markup draws no row here: its work row landed where it was
// delivered (Host.work). A message with neither draws no row.
func queuedRows(msgs []string) []Queued {
	var out []Queued
	for _, m := range msgs {
		if shown, _, _ := userRowsFor(m); shown != "" {
			out = append(out, Queued{Text: shown})
		}
	}
	return out
}

// joinQueue makes queued messages one message with every shown part ahead
// of the first agent.AttachMarker, where each reader cuts the text.
// _queueRideBytes caps what one follow-up turn carries whole: older messages
// are logged, and the view the turn reads already summarizes them, so a
// backlog left overnight cannot blow the turn's window.
// NOTE: the cut is by bytes, not by meaning; the summaries may flatten what
// an earlier message said.
const _queueRideBytes = 64 << 10

// joinQueue joins the queue into one message, the oldest first. The newest
// messages ride whole within queueRideBytes; the ones older than that count
// into one line, since the view already carries them.
func joinQueue(msgs []string) string {
	ride := len(msgs)
	used := 0
	for ; ride > 0; ride-- {
		if used > 0 && used+len(msgs[ride-1]) > _queueRideBytes {
			break
		}
		used += len(msgs[ride-1])
	}
	var shown []string
	var tails strings.Builder
	if ride > 0 {
		shown = append(shown, fmt.Sprintf("%d earlier messages arrived while you worked; the chat's view above already summarizes them.", ride))
	}
	for _, m := range msgs[ride:] {
		head, tail, ok := strings.Cut(m, agent.AttachMarker)
		if ok {
			tails.WriteString(agent.AttachMarker + tail)
		}
		if s := shownText(head); s != "" {
			shown = append(shown, s)
		}
	}
	return strings.Join(shown, "\n\n") + tails.String()
}

// beginTurn resets the per-turn stream state (ag.mu held).
func (ag *agentHost) beginTurn() {
	ag.turns++
	ag.textBuf, ag.reasonBuf = "", ""
	ag.tools = nil
	// no step has started, so a retry before one (compact's) takes nothing
	// back, the last turn's rows included
	ag.stepText, ag.stepReason, ag.stepTools, ag.stepRows, ag.stepThought, ag.stepTurnThought = "", "", nil, math.MaxInt, 0, 0
	ag.stretchMS = nil
	ag.turnStart = ag.clock.Now()
	ag.turnThought = 0
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
	// the tools run on the scope's context: the stream's cancel alone leaves
	// a shell child running to completion on a chat that no longer exists
	if t := h.turn.Load(); t != nil {
		t.cancel()
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
		// the cut rows never settled in the log (their step re-streams), so
		// the pairing waits at the cut
		if h.stampRow > n {
			h.stampRow = n
		}
	}
	h.tMu.Unlock()
	h.refresh()
}

// inputDiff is a file tool's change as its input describes it, before it
// runs. It reads the file: call it outside the locks UI readers share.
func inputDiff(name, input string) string {
	n := tools.ToolName(name)
	if n != tools.ToolWrite && n != tools.ToolEdit {
		return ""
	}
	var in map[string]any
	if json.Unmarshal([]byte(input), &in) != nil {
		return ""
	}
	return capLines(tools.ChangeDiff(n, in), 24)
}

// toolStatus is a finished call's row status, from the outcome its tool
// reported and the result itself: a background hand-off, a call that
// answered with a failure inside, or a stop the user asked for. The only
// chooser of a CallStatus, so no caller can hand it an outcome it made up.
func toolStatus(result fantasy.ToolResultOutputContent, cancelled bool, o tools.Outcome) CallStatus {
	ok := !agent.ErrResult(result)
	switch {
	case ok && (o.Job != "" || o.Run != ""):
		return CallBG
	case ok && o.Warn:
		return CallWarn
	case ok:
		return CallOK
	case o.Stopped || cancelled:
		return CallStopped
	default:
		return CallErr
	}
}

// turnModelID is the model the running turn bills to; a turn the host did
// not start (tests) bills to the default role.
func (h *Host) turnModelID() string {
	h.agents.mu.Lock()
	defer h.agents.mu.Unlock()
	return h.turnModelIDLocked()
}

// turnModelIDLocked is turnModelID with ag.mu held.
func (h *Host) turnModelIDLocked() string {
	if m := h.agents.turnModel; m != "" {
		return m
	}
	return h.roleModel(_defaultRole)
}

// emitStep is agent.step, one per model call of a turn, a subagent's included.
func (h *Host) emitStep(sid, model string, st agent.Step) {
	u := st.Usage
	h.EmitAsync(EventAgentStep, map[string]any{
		"session": sid, "model": model, "step": st.N,
		"ms": st.Took.Milliseconds(), "first_token_ms": st.FirstToken.Milliseconds(), "tools": st.Tools,
		"usage": map[string]any{
			"in": u.InputTokens, "out": u.OutputTokens, "cache_read": u.CacheReadTokens, "cache_write": u.CacheCreationTokens,
			"cost": h.turnCost(model, u),
		},
	})
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
func (h *Host) flushOutput(stat TurnStat) int {
	ag := h.agents
	ag.endThinking()
	var rows []ChatMessage
	if strings.TrimSpace(ag.reasonBuf) != "" {
		d := ag.thought
		rows = append(rows, ChatMessage{
			Role: fantasy.MessageRoleAssistant, Kind: RowReasoning, Text: ag.reasonBuf,
			Stat: &TurnStat{ThoughtMS: d.Milliseconds()},
		})
		// the stretch's time waits for the step's message persist, which logs
		// the row; a retry re-streams the step and takes the queue back with it
		ag.stretchMS = append(ag.stretchMS, d)
	}
	done := slices.IndexFunc(ag.tools, func(t StreamTool) bool { return t.Status == CallRun || t.Status == CallWait })
	if done < 0 {
		done = len(ag.tools)
	}
	for _, t := range ag.tools[:done] {
		rows = append(rows, ChatMessage{
			Role: fantasy.MessageRoleAssistant, Kind: RowTool, CallID: t.ID, Tool: tools.ToolName(t.Name),
			Text: t.Input, Label: t.Label, Prompt: steerPrompt(t.Name, t.Raw), Status: t.Status, Detail: t.Detail,
			Job: t.Job, Run: t.Run, Diff: t.Diff, Output: t.Output,
		})
	}
	if strings.TrimSpace(ag.textBuf) != "" {
		rows = append(rows, ChatMessage{Role: fantasy.MessageRoleAssistant, Text: ag.textBuf, Stat: &stat})
	}
	ag.turnThought += ag.thought
	ag.reasonBuf, ag.tools, ag.textBuf, ag.thought = "", slices.Clone(ag.tools[done:]), "", 0
	h.appendTranscript(rows...)
	h.stampLogSeqs()
	// the flushed rows are in the log: work rows a report parked behind them
	// land after them, in the log and in the window alike
	h.drainWork()
	ag.unflushed = false
	h.refreshLevels()
	return len(rows)
}

// joinReasoning appends a late reasoning chunk to the row the thought it
// continues settled as: a provider sends a thought's last token after the call
// that interrupted it started. It joins only a transcript whose tail row is
// that thought; a row settled after it makes the chunk a thought of its own.
func (h *Host) joinReasoning(text string) bool {
	h.tMu.Lock()
	defer h.tMu.Unlock()
	n := len(h.session.Transcript)
	if n == 0 || h.session.Transcript[n-1].Kind != RowReasoning {
		return false
	}
	tr := slices.Clone(h.session.Transcript)
	tr[n-1].Text += text
	// replaced whole, like setTranscript: readers share the old array and the
	// chat widget caches rows by index
	h.session.Transcript = tr
	h.trEdit.Add(1)
	return true
}

// appendAfterLive adds m to the transcript where the turn has got to: after
// the live rows that finished, which move there first, and above the calls
// still running.
func (h *Host) appendAfterLive(m ChatMessage) {
	if m.Seq == 0 { // a live row names no log message until the stamping pairs it
		m.Seq = NoSeq
	}
	ag := h.agents
	ag.mu.Lock()
	h.flushOutput(TurnStat{})
	h.tMu.Lock()
	h.session.Transcript = h.trimTranscript(append(h.session.Transcript, m))
	h.tMu.Unlock()
	ag.mu.Unlock()
	h.refresh()
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
// input. A question call is its headers, so its row never repeats the
// question it shows below.
func toolLabel(input string) string {
	var in struct {
		About     string `json:"about"`
		Questions []struct {
			Header string `json:"header"`
		} `json:"questions"`
	}
	if json.Unmarshal([]byte(input), &in) != nil {
		return ""
	}
	if n := len(in.Questions); n > 0 {
		var heads []string
		for _, q := range in.Questions {
			if h := clipLine(q.Header, 80); h != "" {
				heads = append(heads, listDesc(h))
			}
		}
		if heads == nil {
			return Plural(n, "a question", fmt.Sprintf("%d questions", n))
		}
		return clipLine(strings.Join(heads, ", "), 80)
	}
	return clipLine(in.About, 80)
}

// toolSummary picks the argument a person reads first (a command, path or
// query) out of a tool's JSON input; anything else stays raw. Paths read
// through rel (a Host's relPath: project-relative inside, as given outside);
// nil keeps them raw.
func toolSummary(rel func(string) string, tool, input string) string {
	if rel == nil {
		rel = func(p string) string { return p }
	}
	var m map[string]any
	if json.Unmarshal([]byte(input), &m) != nil {
		return input
	}
	if len(m) == 0 { // an empty call says nothing: "{}" is noise
		return ""
	}
	if _, ok := m["items"]; ok {
		var in struct {
			Items []store.Todo `json:"items"`
		}
		_ = json.Unmarshal([]byte(input), &in) // the map decoded, so this does too
		done := 0
		for _, td := range in.Items {
			if td.Status == store.TodoDone {
				done++
			}
		}
		return fmt.Sprintf("%d %s, %d done", len(in.Items), Plural(len(in.Items), "todo", "todos"), done)
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
	if tools.ToolName(tool) == tools.ToolWorkflow { // the run as /workflows names it, else what a control does to which run
		str := func(k string) string { s, _ := m[k].(string); return s }
		if label := flowLabel(str("description"), str("name"), str("script")); label != "" || str("run") == "" {
			return cmp.Or(label, "workflow")
		}
		return cmp.Or(str("action"), string(_actStatus)) + " " + str("run")
	}
	if tools.ToolName(tool) == tools.ToolLua { // the about is its label, and a script reads as nothing short
		path, _ := m["path"].(string)
		if path == "" {
			return ""
		}
		return rel(path)
	}
	if name := tools.ToolName(tool); name == tools.ToolZoom || name == tools.ToolDate { // the line it names, in the id+n shape the chat's summaries use
		var in zoomArgs // date's input is its id alone, which zoomArgs carries the same way
		if json.Unmarshal([]byte(input), &in) == nil {
			id := strconv.FormatInt(in.ID, 10)
			switch {
			case name == tools.ToolDate:
				return id
			case in.N > 1:
				return id + "+" + strconv.FormatInt(in.N, 10)
			case in.Page > 0:
				return fmt.Sprintf("%s page %d", id, in.Page+1)
			default:
				return id
			}
		}
	}
	// a tool that names what it does in "action" reads as the action and
	// what it acts on, not the whole body of what it is about to show
	if action, ok := m["action"].(string); ok && action != "" {
		for _, k := range []string{"symbol", "path", "id"} {
			if v, ok := m[k]; ok {
				if s := fmt.Sprint(v); s != "" {
					if k == "path" {
						s = rel(s)
					}
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
		return strings.TrimSuffix(a+": "+clipLine(prompt, 80), ": ")
	}
	for _, k := range []string{"path", "pattern", "url", "query", "question", "name", "text", "description", "id"} {
		if s, ok := m[k].(string); ok && s != "" {
			if k == "path" {
				s = rel(s)
			}
			return s
		}
	}
	// any other tool (an MCP one): its first string argument, in the order
	// the model wrote them
	dec := jsontext.NewDecoder(strings.NewReader(input))
	_, _ = dec.ReadToken() // the object's opening brace
	for dec.PeekKind() != jsontext.Kind('}') {
		if _, err := dec.ReadToken(); err != nil {
			break
		}
		var v any
		if err := json.UnmarshalDecode(dec, &v); err != nil {
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

// storeMessage writes one message as a row of session sid, and for a
// reasoning message the thinking-time row beside it: the message does not
// keep that, and historyFrom reads it back from the row.
func (h *Host) storeMessage(sid string, msg fantasy.Message, thought time.Duration) (*store.Message, error) {
	parts, err := json.Marshal(msg, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	row := &store.Message{SessionID: sid, Role: string(msg.Role), PartsJSON: string(parts)}
	if rows := h.takeCallRows(msg); len(rows) > 0 {
		// the calls this message drew, stored beside it for the run's
		// transcript rows
		if meta, err := json.Marshal(rows, json.Deterministic(true)); err == nil {
			row.RowMeta = string(meta)
		}
	}
	if err := h.DB.AddMessage(row); err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(msg.Content, func(p fantasy.MessagePart) bool { return p.GetType() == fantasy.ContentTypeReasoning }) {
		return row, nil
	}
	parts, _ = json.Marshal(map[string]any{"thought_ms": thought.Milliseconds()}, json.Deterministic(true))
	if err := h.DB.AddMessage(&store.Message{SessionID: sid, Role: "tool", PartsJSON: string(parts)}); err != nil {
		return nil, err
	}
	return row, nil
}

// persistMessage persists one main-chat message into the eternal log: the
// user's words, replies, thought rows, tool calls and results. A failure
// costs the log a line, never the turn. A run's session keeps its
// own rows, wired on the run's runner.
// logAppendFailed is the one Warn a failed chat-log append makes; the call
// sites share it so the message lives once.
func (h *Host) logAppendFailed(err error) {
	h.Log.Warn("chat log append failed", "origin", "agent", "error", err)
}

func (h *Host) persistMessage(msg fantasy.Message) error {
	if h.chat == nil {
		return nil
	}
	seq, err := h.logChatMessage(msg)
	if err != nil {
		h.logAppendFailed(err)
		return nil
	}
	// the log holds rows the window has not drawn: a work row arriving before
	// the next flush waits for it, so the two keep one order
	h.agents.mu.Lock()
	h.agents.unflushed = true
	h.agents.mu.Unlock()
	if seq >= 0 {
		h.stampLogSeqs()
	}
	if msg.Role == fantasy.MessageRoleUser && seq >= 0 {
		h.setGoalFrom(seq, msg)
	}
	return nil
}

// logChatMessage appends one main-chat message to the eternal log and
// returns the first seq it wrote, -1 for none.
func (h *Host) logChatMessage(msg fantasy.Message) (int64, error) {
	switch msg.Role {
	case fantasy.MessageRoleUser:
		typed, _ := agent.CutAttached(concatText(msg.Content))
		if strings.TrimSpace(typed) != "" {
			return h.chat.User(typed, logImages(msg))
		}
	case fantasy.MessageRoleAssistant:
		return -1, h.logAssistant(msg)
	case fantasy.MessageRoleTool:
		return -1, h.logResults(msg)
	case fantasy.MessageRoleSystem:
	}
	return -1, nil
}

// logAssistant logs a step's reply, its thought rows and its tool calls
// apart: the text as one agent row per stretch, each call its own tool row,
// each reasoning stretch the thought row the live turn drew, in content
// order.
func (h *Host) logAssistant(msg fantasy.Message) error {
	var text strings.Builder
	flush := func() error {
		defer text.Reset()
		if strings.TrimSpace(text.String()) == "" {
			return nil
		}
		return h.chat.Agent(text.String())
	}
	for _, p := range msg.Content {
		switch p := p.(type) {
		case fantasy.ReasoningPart:
			if strings.TrimSpace(p.Text) == "" {
				break
			}
			if err := h.logThought(p.Text); err != nil {
				return err
			}
		case fantasy.TextPart:
			text.WriteString(p.Text)
		case fantasy.ToolCallPart:
			if err := flush(); err != nil {
				return err
			}
			if err := h.chat.Tool(p.ToolName, p.Input, p.ToolCallID); err != nil {
				return err
			}
		}
	}
	return flush()
}

// logThought logs one reasoning stretch with the wall time its flush
// measured, oldest first; a stretch no call followed never flushed, and
// reads the thought still open, closed here for the log.
func (h *Host) logThought(text string) error {
	var d time.Duration
	ag := h.agents
	ag.mu.Lock()
	if n := len(ag.stretchMS); n > 0 {
		d, ag.stretchMS = ag.stretchMS[0], ag.stretchMS[1:]
	} else if ag.thinking || ag.thought > 0 {
		ag.endThinking()
		d = ag.thought
	}
	ag.mu.Unlock()
	return h.chat.Thought(text, d)
}

// logResults logs a step's tool results as echo rows, each named by the call
// it answers and carrying the row the call settled at, so a window rebuilt
// from the log draws what the live turn drew.
func (h *Host) logResults(msg fantasy.Message) error {
	for _, p := range msg.Content {
		res, ok := p.(fantasy.ToolResultPart)
		if !ok {
			continue
		}
		ag := h.agents
		ag.mu.Lock()
		row, found := StreamTool{}, false
		if i := slices.IndexFunc(ag.tools, func(t StreamTool) bool { return t.ID == res.ToolCallID }); i >= 0 {
			row, found = ag.tools[i], true
		}
		ag.mu.Unlock()
		if !found {
			// a flush pruned the call already: the result's own ok is all there is
			row = StreamTool{ID: res.ToolCallID}
		}
		if err := h.chat.Echo(h.callName(res.ToolCallID), !agent.ErrResult(res.Output), agent.ToolOutput(res.Output), row); err != nil {
			return err
		}
	}
	return nil
}

// logImages marshals a user message's images for the log, so zoom replays
// them; "" when the message carries none.
func logImages(msg fantasy.Message) string {
	var imgs []fantasy.FilePart
	for _, p := range msg.Content {
		if f, ok := p.(fantasy.FilePart); ok {
			imgs = append(imgs, f)
		}
	}
	if imgs == nil {
		return ""
	}
	b, err := json.Marshal(imgs, json.Deterministic(true))
	if err != nil {
		return ""
	}
	return string(b)
}

// callName is the tool a running turn's call id belongs to, "" once its row
// is gone.
func (h *Host) callName(id string) string {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	for _, t := range ag.tools {
		if t.ID == id {
			return t.Name
		}
	}
	return ""
}

// recordToolCall stores one tool telemetry row in session sid (each tool
// call gets its own "tool" message row so tool_calls.message_id stays a
// real FK). The output is not kept: the history row already holds it. A
// file tool's diff is, keyed by call id: it is computed against the file
// as it was, and the run's zoom reads it whole; so is an agent call's run
// and summary line.
func (h *Host) recordToolCall(sid string, t StreamTool, ok bool) {
	if h.DB == nil || sid == "" {
		return
	}
	row := map[string]any{"tool": t.Name}
	if t.Diff != "" || t.Run != "" {
		row["id"], row["diff"], row["run"], row["detail"] = t.ID, t.Diff, t.Run, t.Detail
	}
	parts, _ := json.Marshal(row, json.Deterministic(true))
	msg := &store.Message{SessionID: sid, Role: "tool", PartsJSON: string(parts)}
	if err := h.DB.AddToolCall(msg, &store.ToolCall{Tool: t.Name, OK: ok}); err != nil {
		h.notKept("tool call not kept", err)
	}
}

// _todoNagAfter is how many of the main agent's steps an in_progress todo
// may stay static before zemi nudges the model to mark it done (a "force live
// todos" remind). Steps, not calls: the prompt asks for independent calls
// in one batch, so one step may be eight reads.
// NOTE: a fixed knob, not config; every todos write resets the count.
const _todoNagAfter = 4

// armTodoTurnNote arms a turn-start remind while the todo list has open
// items: a long session's view carries the list only as a summary line, and
// a model that never opens it works untracked. One note per turn, dropped by
// the todos write the turn starts with. The store is the list's truth, so
// the note reads it fresh.
func (h *Host) armTodoTurnNote() {
	var todos []store.Todo
	if h.DB != nil {
		todos, _ = h.DB.Todos(h.AgentSessionID())
	}
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if ag.todoNag != "" || len(todos) == 0 {
		return
	}
	open := 0
	for _, td := range todos {
		if td.Status != store.TodoDone {
			open++
		}
	}
	if open == 0 {
		return
	}
	ag.todoNag = agent.Note(fmt.Sprintf(
		"%d %s open from earlier: keep tracking this work with the todos tool, marking the one you take up in_progress.",
		open, Plural(open, "todo is", "todos are"),
	))
}

// maybeNagTodos counts a main-runner step against a stale todo list and
// nudges once per period while the list stays stale: the model is free to
// forget, and a todos write re-arms the tracker. It rides the model-only
// steering channel (agent.AttachMarker), so it never reads as the user's own
// message. Only the main agent's steps during its turn count: a subagent's
// would arm the nag while nobody can be nudged.
func (h *Host) maybeNagTodos() {
	// a call esc ended arms nothing: PrepareStep would persist the nag for
	// a step that never runs, ahead of the user's next message
	if t := h.turn.Load(); t != nil && t.ctx.Err() != nil {
		return
	}
	ag := h.agents
	ag.mu.Lock()
	if !ag.active || ag.todoPending {
		ag.mu.Unlock()
		return
	}
	current, open := "", 0
	for _, td := range ag.todoList {
		switch td.Status {
		case store.TodoInProgress:
			current = td.Content
		case store.TodoPending:
			open++
		}
	}
	if current == "" && open == 0 {
		ag.mu.Unlock()
		return // nothing open: nothing to nudge about
	}
	ag.todoSteps++
	if ag.todoSteps-ag.todoNagAt < _todoNagAfter {
		ag.mu.Unlock()
		return
	}
	ag.todoNagAt = ag.todoSteps
	ag.mu.Unlock()
	// the store is the truth at the moment of the nag: a sibling's ticks
	// never reach this window's cache mid-turn, and no store call runs under
	// the agents' lock
	todos := ag.todoList
	if h.DB != nil {
		if td, err := h.DB.Todos(h.AgentSessionID()); err == nil && len(td) > 0 {
			todos = td
		}
	}
	ag.mu.Lock()
	defer ag.mu.Unlock()
	current, open = "", 0
	for _, td := range todos {
		switch td.Status {
		case store.TodoInProgress:
			current = td.Content
		case store.TodoPending:
			open++
		}
	}
	if current == "" && open == 0 {
		return // the sibling finished it: nothing left to nudge about
	}
	ag.todoPending = true
	if current != "" {
		ag.todoNag = agent.Note(fmt.Sprintf(
			"%q has been in_progress for %d steps. If it is done, mark it done with the todos tool and set the next item in_progress; if not, carry on.",
			current, ag.todoSteps,
		))
		return
	}
	ag.todoNag = agent.Note(fmt.Sprintf(
		"%d %s pending and none is in_progress: set the one you are working on in_progress with the todos tool.",
		open, Plural(open, "todo is", "todos are"),
	))
}

// resetTodoNag re-arms the staleness tracker from a fresh todo write. A
// next turn's first todos write is what started counting, so this also stops
// the nag whenever the model engages at all.
func (h *Host) resetTodoNag(todos []store.Todo) {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	// the armed note dies with the list: a write makes it stale, a wipe
	// must not let it ride into the chat that replaced the old one
	ag.todoNag = ""
	ag.todoSteps, ag.todoNagAt, ag.todoPending = 0, 0, false
	if len(todos) < 2 {
		ag.todoList = nil // a single item is a trivial task: it never nags
		return
	}
	ag.todoList = slices.Clip(todos)
}

// ensureSession ran on the first send; the chat has no session row now, but
// the plugins' SessionStart hooks still capture once, at the first send.
func (h *Host) captureSessionStart() {
	if h.sessionStartDone.Swap(true) {
		return
	}
	h.captureSessionContext()
}

// _btwPrompt frames a side question, after maki's: nothing it says is kept,
// so the answer has to come from what the conversation already holds.
const _btwPrompt = `Side question. Answer only it, in this one reply, from what the conversation already shows. Call no tools: none will run. If the conversation does not tell you, say so.

Question: `

// Btw runs /btw: a side question answered in an overlay from the
// conversation so far, a running turn included, and never stored. It is a
// busy job, which a double esc cancels.
func (h *Host) Btw(question string) error {
	ui := h.hooks()
	h.GoSafe("btw", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer h.Busy("Answering the side question", cancel)()
		answer, err := h.btw(ctx, question)
		switch {
		case ctx.Err() != nil:
		case errors.Is(err, errNothingToAsk):
			h.Notify(slog.LevelWarn, "Nothing to ask about yet", "")
		case err != nil:
			h.Notify(slog.LevelError, "Could not answer the side question", h.friendlyError(err))
		default:
			ui.OpenOverlay(Overlay{ID: fmt.Sprintf("btw:%d", h.selectSeq.Add(1)), Title: "Side question", Origin: lua.OriginBuiltin, Body: answer, BodyMD: true})
		}
	})
	return nil
}

var errNothingToAsk = errors.New("no conversation to ask about yet")

func (h *Host) btw(ctx context.Context, question string) (string, error) {
	answer, err := h.aside(ctx, _btwPrompt+question)
	return cmp.Or(answer, "(no answer: the model reached for a tool)"), err
}

// aside sends text on the prefix the next send would use (prompt, tools), so
// it reads the provider's cache, and bills it. A reply that is
// only a tool call is "".
func (h *Host) aside(ctx context.Context, text string) (string, error) {
	ag := h.agents
	ag.mu.Lock()
	live := ag.runner
	ag.mu.Unlock()
	// a settled turn left no runner: the conversation is its view, the same
	// message the next send would carry, so the answer still reads the chat
	var history []fantasy.Message
	if live != nil {
		history = live.Snapshot()
	} else if msg, ok := h.viewMessage(); ok {
		history = []fantasy.Message{msg}
	} else {
		return "", errNothingToAsk
	}
	r, err := h.ensureRunner(h.mainTools())
	if err != nil {
		return "", err
	}
	if live != nil {
		r.Continue(live) // what live last sent decides which thinking still holds
	}
	res, err := r.Aside(ctx, history, text)
	if err != nil {
		return "", err
	}
	h.bill(store.MainSessionID, h.roleModel(_defaultRole), res.TotalUsage)
	return strings.TrimSpace(concatText(res.Response.Content)), nil
}

const _recapPrompt = `The user stepped away and is coming back. In one or two short sentences of plain text, say what this conversation is working toward, then where it stands: the next step, a question waiting on the user, or a check not yet run. Keep file, command and identifier names exact. Call no tools: none will run. No heading, no label, no apology.`

// Recap adds a notice row saying where the session stands, from an aside on
// the cached conversation. The UI calls it when nobody touched the terminal
// since a turn ended well, and cancels ctx on the next input; it is a busy
// job too, so esc stops it and the working row names it.
func (h *Host) Recap(ctx context.Context) {
	h.GoSafe("recap", func() {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		defer h.Busy("Recapping", cancel)()
		h.recap(ctx)
	})
}

func (h *Host) recap(ctx context.Context) {
	if h.Settings.Duration(config.KeySessionRecap) <= 0 {
		return
	}
	ag := h.agents
	ag.mu.Lock()
	idle := ag.runner != nil && !ag.active && h.tasks.runningFor(store.MainSessionID) == 0
	ag.mu.Unlock()
	h.tMu.RLock()
	tr := h.session.Transcript
	h.tMu.RUnlock()
	// a turn that ran no tool is its own recap
	worked := false
	for _, m := range slices.Backward(tr) {
		worked = m.Kind == RowTool
		if worked || m.Role == fantasy.MessageRoleUser && m.Kind == RowMessage {
			break
		}
	}
	if !idle || !worked {
		return
	}
	answer, err := h.aside(ctx, _recapPrompt)
	lines := slices.DeleteFunc(strings.Split(answer, "\n"), func(l string) bool { return strings.TrimSpace(l) == "" })
	text := clipLine(strings.Join(lines[:min(2, len(lines))], " "), 300)
	if err != nil || text == "" {
		h.Log.Debug("no recap", "origin", "session", "error", fmt.Sprint(err))
		return
	}
	// the first recap ever names its /config key
	detail := ""
	if h.GlobalDB != nil {
		if _, err := h.GlobalDB.GetKV("recap.hinted"); errors.Is(err, store.ErrNotFound) {
			detail = "turn off in /config"
		}
	}
	// a send, a report or a reset since the call would leave it stale
	h.tMu.RLock()
	added := ctx.Err() == nil && len(h.session.Transcript) == len(tr)
	h.tMu.RUnlock()
	if !added {
		return
	}
	h.notice("Recap: "+text, detail)
	if detail != "" {
		_ = h.GlobalDB.SetKV("recap.hinted", "true")
	}
	h.refresh()
}

// AgentSessionID is the id every main-chat surface names: the anchor row the
// migration plants, which billing and todos ride.
func (h *Host) AgentSessionID() string { return store.MainSessionID }

// beginThinking opens a reasoning block (ag.mu held); endThinking closes
// any open block into the accumulated thought time. agentRun holds a twin
// for child runs: both flushes must produce the stream order historyFrom
// decodes.
func (ag *agentHost) beginThinking() {
	if !ag.thinking {
		ag.thinking = true
		ag.thinkStart = ag.clock.Now()
	}
}

func (ag *agentHost) endThinking() {
	if ag.thinking {
		d := ag.clock.Now().Sub(ag.thinkStart)
		ag.thought += d
		ag.stepThinking += d
		ag.thinking = false
	}
}

// namespaces

// strs is a closed set's values as plain strings.
func strs[T ~string](vals []T) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = string(v)
	}
	return out
}

// OpenModelPicker lists the registered models for a target, where enter sets
// it, then the keyless builtin providers, where it copies the variable.
// Name "" is /model and "default" the model key's list, both of them the
// conversation's: a pick there writes models.default. Another
// target's list opens on a row leaving it to its own model.
func (h *Host) OpenModelPicker(name string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	ui.OpenOverlay(h.modelPicker(name))
	return nil
}

// followRow is a target's list head: what it runs on with no pick of its own.
func (h *Host) followRow(name, run string) OverlayItem {
	if name == _defaultRole { // the root: nothing to follow but the provider
		return OverlayItem{Text: "Follow the first usable model", Desc: h.Providers.Default(), Run: run + _defaultRole}
	}
	head := OverlayItem{Text: "Follow the default model", Desc: h.roleModel(_defaultRole), Run: run + _defaultRole}
	if t, _ := h.target(name); t != nil && t.def() != "" {
		head.Text, head.Desc = "Follow the registered model", h.modelRef(t.def())
	}
	return head
}

func (h *Host) modelPicker(name string) Overlay {
	// a model id is the row's identity and the descs run long, so the box
	// measures the lists' own width and nothing trims to a menu's width
	o := Overlay{ID: "model", Title: "Model", Origin: lua.OriginBuiltin, Anchor: OverlayBottomRight, Verb: "pick", Empty: "No model yet: " + h.addProvider() + "."}
	// a bare pick writes the conversation's slot, so that is the row the lists
	// without a target of their own mark: /model and /model chat
	run, cur := "model ", h.roleModel(_defaultRole)
	switch name {
	case "":
		o.Items = []OverlayItem{
			{Text: "Choose a model per role", Desc: rolesDesc(h.ownRoles()), Verb: VerbOpen, Run: "model roles"},
			{Text: "Update the model catalog", Desc: h.Builtin.Load().Info.String(), Verb: VerbRun, Run: "model update"},
		}
	default:
		o.ID, o.Title, run, cur = "model "+name, "Model roles / "+name, "model "+name+" ", h.Settings.Str(config.ModelKey(name))
		head := h.followRow(name, run)
		if cur == "" {
			head.Tag = "current"
		}
		o.Items = []OverlayItem{head}
	}
	heads := len(o.Items)
	// a decisions model answers classification only, so it is offered to the
	// slots that classify and never to a chat one
	classifier := h.classifies(name)
	// a favorited model heads the list, in the order the key holds them
	favs := h.Settings.List(config.KeyModelsFavorites)
	type modelRow struct {
		id   string
		item OverlayItem
	}
	var rows []modelRow
	for _, s := range h.Providers.List() {
		section := s.ID
		if !h.Providers.HasKey(s.ID) {
			section += "  no API key"
		}
		for _, m := range s.Models {
			if m.Decisions && !classifier {
				continue
			}
			id := s.ID + "/" + m.ID
			// the id is the row: a rebuild keeps the selection on it, and f
			// marks the model it names
			item := OverlayItem{ID: id, Text: m.ID, Section: section, Desc: modelDesc(id, m), Run: run + id, Copy: id}
			if id == cur {
				item.Tag = "current"
			}
			rows = append(rows, modelRow{id, item})
		}
	}
	// a favorited model heads the list as a copy of itself: the row also
	// stays in its provider's section below, where its provider groups it
	for _, id := range favs {
		if i := slices.IndexFunc(rows, func(r modelRow) bool { return r.id == id }); i >= 0 {
			fav := rows[i].item
			fav.Section = "Favorites"
			o.Items = append(o.Items, fav)
		}
	}
	for _, r := range rows {
		o.Items = append(o.Items, r.item)
	}
	// the selection opens on the model the slot runs
	o.Selected = slices.IndexFunc(rows, func(r modelRow) bool { return r.id == cur })
	if o.Selected >= 0 {
		o.Selected += heads
	}
	prov := agent.ModelID(cur).Provider()
	_, registered := h.Providers.Lookup(prov)
	if _, known := h.modelSpec(cur); cur != "" && !known && registered {
		// config named it by hand: shown so the typo is in view
		o.Items = slices.Insert(o.Items, heads, OverlayItem{ID: cur, Text: cur, Run: run + cur, Tag: "current, not in the list", Desc: "the provider decides whether it exists"})
		o.Selected = heads
	}
	// f marks the model a row names, wherever it sits in the list
	o.Keys = append(o.Keys, OverlayKey{
		Key: "f", Desc: "favorite", Applies: func(it OverlayItem) bool { return it.ID != "" },
		Act: func(it OverlayItem) { h.toggleModelFavorite(it.ID, name) },
	})
	usable := len(o.Items) > heads
	if !usable {
		o.Items = nil
	}
	// the builtin providers without a key close the list, so a user finds
	// what else zemi supports and the variable that turns each on; enter
	// pastes a key, y still copies the variable
	for _, m := range h.Providers.Missing() {
		o.Items = append(o.Items, missingKeyRow(m))
	}
	if !usable && len(o.Items) > 0 {
		// the init.lua path is split at its slashes, so a narrow box wraps it
		// whole-component instead of mid-word; Body stays the line y copies
		segs := []Segment{{Text: "No model yet: press enter on a provider below to paste its key, export one in your shell and start zemi again, or add a provider to "}}
		for part := range strings.SplitAfterSeq(h.initPath(), "/") {
			segs = append(segs, Segment{Text: part})
		}
		segs = append(segs, Segment{Text: "."})
		// no key export fixes a default whose provider nobody registered:
		// name it, so the user knows what to change
		if id := h.Settings.Str(config.KeyModelsDefault); id != "" {
			p := agent.ModelID(id).Provider()
			_, registered := h.Providers.Lookup(p)
			awaiting := slices.ContainsFunc(h.Providers.Missing(), func(m agent.MissingKey) bool { return m.ID == p })
			if !registered && !awaiting {
				segs = append(segs, Segment{Text: " models.default is " + id + ", whose provider is not registered."})
			}
		}
		o.BodySegs, o.Body = [][]Segment{segs}, SegsText(segs)
	}
	return o
}

// refreshModelPickers rebuilds the open model pickers, the selection on
// provider's first model where one is named - where a paste from its
// Needs-a-key row lands. The ids are modelPicker's own.
func (h *Host) refreshModelPickers(provider string) {
	ui := h.hooks()
	if ui == nil {
		return
	}
	p := provider
	ui.RefreshOverlay("model", func() Overlay { return h.modelPickerAt("", p) })
	for _, name := range h.targetNames() {
		ui.RefreshOverlay("model "+name, func() Overlay { return h.modelPickerAt(name, p) })
	}
}

// modelPickerAt is the picker with the selection on provider's first model.
func (h *Host) modelPickerAt(name, provider string) Overlay {
	o := h.modelPicker(name)
	if provider == "" {
		return o
	}
	if i := slices.IndexFunc(o.Items, func(it OverlayItem) bool { return agent.ModelID(it.ID).Provider() == provider }); i >= 0 {
		o.Selected = i
	}
	return o
}

// toggleModelFavorite marks or unmarks a model in models.favorites, then
// rebuilds the picker under the key, which keeps the selection on the row.
func (h *Host) toggleModelFavorite(id, name string) {
	favs := slices.Clone(h.Settings.List(config.KeyModelsFavorites))
	if i := slices.Index(favs, id); i >= 0 {
		favs = slices.Delete(favs, i, i+1)
	} else if id != "" {
		favs = append(favs, id)
	}
	if _, err := h.writeSetting(config.KeyModelsFavorites, favs); err != nil {
		h.Notify(slog.LevelWarn, "Could not save the favorite", err.Error())
		return
	}
	// the box the key was pressed in is gone by now: this is a fresh list,
	// opened on the row the key marked wherever it moved
	o := h.modelPicker(name)
	if i := slices.IndexFunc(o.Items, func(it OverlayItem) bool { return it.ID == id }); i >= 0 {
		o.Selected = i
	}
	if ui := h.hooks(); ui != nil {
		ui.OpenOverlay(o)
	}
}

// ownRoles names the roles and targets config gave a model of their own,
// the default root apart: they are the picks that leave the shared model.
func (h *Host) ownRoles() []string {
	var own []string
	for _, n := range h.targetNames() {
		if n != _defaultRole && h.Settings.Str(config.ModelKey(n)) != "" {
			own = append(own, n)
		}
	}
	return own
}

// rolesDesc is the desc of /model's row opening the roles: which roles leave the default model.
func rolesDesc(own []string) string {
	switch n := len(own); n {
	case 0:
		return "every role on the default model"
	case 1:
		return own[0] + " on its own model"
	default:
		return strings.Join(own[:n-1], ", ") + " and " + own[n-1] + " on their own model"
	}
}

// OpenModelRoles lists the conversation's default slot, the roles and the
// slots config registered with the model each runs on. Enter opens a slot's
// model list over it, and the list
// rebuilds when that one closes.
func (h *Host) OpenModelRoles() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	build := h.modelRoleItems
	items := build()
	ui.OpenOverlay(Overlay{ID: "model roles", Title: "Model roles", Origin: lua.OriginBuiltin, Items: items, Anchor: OverlayBottomRight, Reload: build})
	return nil
}

// modelRoleItems is /model roles: the conversation's default slot first, then
// the other roles, then the slots config registered.
func (h *Host) modelRoleItems() []OverlayItem {
	items := []OverlayItem{{
		Text: _defaultRole, Desc: h.slotLine(_defaultRole, _roles[0].Desc),
		Verb: VerbOpen, Run: "model " + _defaultRole,
	}}
	for _, r := range _roles[1:] {
		items = append(items, h.modelSlotRow(r.Name, r.Desc))
	}
	for _, t := range h.registeredTargets() {
		items = append(items, h.modelSlotRow(t.Name, t.Desc))
	}
	return items
}

// modelSlotRow is one /model roles row: what /model <name> opens, and the tag
// naming what a slot with no pick of its own falls back to.
func (h *Host) modelSlotRow(name, desc string) OverlayItem {
	item := OverlayItem{
		Text: name, Desc: h.slotLine(name, desc),
		Verb: VerbOpen, Run: "model " + name,
	}
	if tag := h.slotTag(name); tag != "" {
		item.Tag = tag
	}
	return item
}

// slotTag is what a row shows past the model id: the default a slot with no
// pick of its own falls back to.
func (h *Host) slotTag(name string) string {
	if name == _defaultRole { // the root: what runs when nothing is set anywhere
		return h.Providers.Default()
	}
	if h.Settings.Str(config.ModelKey(name)) != "" {
		return ""
	}
	if def := h.targetDef(name); def != "" {
		return h.modelRef(def)
	}
	return "follows default"
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
		// both sides to the cent: a mixed precision reads as two scales
		parts = append(parts, "$"+strconv.FormatFloat(m.CostPer1MIn, 'f', 2, 64)+"/$"+strconv.FormatFloat(m.CostPer1MOut, 'f', 2, 64))
	}
	return strings.Join(parts, "   ")
}

// SetModel runs /model's arguments from any goroutine: none opens the chat
// list, "roles" the role list, "update" fetches the catalog, a role its
// list, an id sets the conversation, and a role with an id, or "default" to
// follow it, sets that role. The config watches it fires run later on the interpreter
// queue. An id the provider does not list is set and toasted.
func (h *Host) SetModel(args string) error {
	f := strings.Fields(args)
	picker := func(role string) error {
		err := h.OpenModelPicker(role)
		if errors.Is(err, ErrNeedsTUI) {
			err = fmt.Errorf("%w; /model [<role>] <id> sets one", err)
		}
		return err
	}
	switch {
	case len(f) == 0:
		return picker("")
	case len(f) == 1 && f[0] == "roles":
		return h.OpenModelRoles()
	case len(f) == 1 && f[0] == "update":
		return h.updateCatalog()
	case len(f) == 1 && h.isTarget(f[0]):
		return picker(f[0])
	case len(f) == 1: // a bare id sets the conversation's slot
		f = []string{_defaultRole, f[0]}
	case len(f) > 2 || !h.isTarget(f[0]):
		return Usagef("usage: /model [roles | update | <target> | [<target>] provider/model | <target> default]; targets: %s", strings.Join(h.targetNames(), ", "))
	}
	return h.SetTargetModel(f[0], f[1])
}

// SetTargetModel sets one target's model: "default" (or "") follows models.default.
// It validates, writes through the watched setting and shows the pick; Go
// callers with a role and id in hand call it directly.
func (h *Host) SetTargetModel(role, id string) error {
	if id == _defaultRole { // a target following its own model, else the default
		id = ""
	}
	if err := h.modelSetErr(role, id); err != nil {
		return err
	}
	if err := h.setWatched(config.ModelKey(role), id); err != nil {
		return err
	}
	if ui, err := h.Screen(); err == nil { // a /model under the roles list shows the pick when it comes back
		o := h.modelPicker("")
		o.Refresh = true
		ui.OpenOverlay(o)
	}
	if id != "" {
		h.warnUnlisted(id)
	}
	return nil
}

// updateCatalog fetches the catwalk catalog for this and later boots as a
// busy job, and reloads config so the registry takes it, even on a 304:
// another zemi may have saved the file this one never ran.
func (h *Host) updateCatalog() error {
	if _, err := h.Screen(); err != nil {
		return fmt.Errorf("%w; /model update fetches the catalog", err)
	}
	kept := "bundled"
	if !h.Builtin.Load().Info.Fetched.IsZero() {
		kept = "saved"
	}
	h.GoSafe("model update", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := func() error {
			defer h.Busy("Updating the model catalog", cancel)()
			c, changed, err := agent.UpdateCatalog(ctx, agent.CatalogURL, h.Dirs.DataHome, os.Getenv)
			if err != nil {
				return fmt.Errorf("%w; the %s one stays", err, kept)
			}
			h.Builtin.Store(&c)
			if changed {
				h.Notify(slog.LevelInfo, "Model catalog updated", c.Info.Summary())
			} else {
				h.Notify(slog.LevelInfo, "Model catalog is up to date", "")
			}
			return nil
		}()
		switch {
		case err == nil:
			_ = h.ReloadConfig()
		case ctx.Err() == nil: // not esc
			h.Notify(slog.LevelError, "Could not update the model catalog", err.Error())
		}
	})
	return nil
}

// CompleteModel offers /model's arguments in SetModel's grammar.
func (h *Host) CompleteModel(lead, line string) []string {
	first, ids := []string{"roles", "update"}, h.ModelIDs()
	first = append(first, h.targetNames()...)
	return SubcommandCompleter(append(first, ids...), func(_ *Host, sub string) []string {
		switch {
		case sub == _defaultRole:
			return ids
		case h.isTarget(sub):
			return append([]string{_defaultRole}, ids...)
		default:
			return nil
		}
	})(h, lead, line)
}

const _unlistedTitle = "Model not listed"

// warnUnlisted toasts a pick the provider does not list. It reads the pick,
// not the model row: mid-turn the row is the running turn's model.
func (h *Host) warnUnlisted(id string) {
	if _, ok := h.modelSpec(id); !ok {
		detail := unlistedDetail(id)
		if near := DidYouMean(id, h.ModelIDs()); near != "" {
			detail += "; did you mean " + near + "?"
		}
		h.Notify(slog.LevelWarn, _unlistedTitle, detail)
	}
}

// modelSetErr refuses a model pick under a provider nobody registered. An id
// the provider does not list is taken and warned about, like -m and config: the
// provider decides. A decisions model is refused for every slot but a
// classifier one: it is no chat model, and that slot is what it is for.
func (h *Host) modelSetErr(slot, id string) error {
	prov := agent.ModelID(id).Provider()
	if _, ok := h.Providers.Lookup(prov); !ok && id != "" {
		return usageError{h.modelErr(id)}
	}
	if _, _, decisions := h.Providers.Decisions(agent.ModelID(id)); decisions && !h.classifies(slot) {
		return usageError{h.modelErr(id)}
	}
	return nil
}

// classifies reports whether a slot was registered for classification
// (zemi.model.add's classify), which is the one kind of slot a decisions model
// may be picked for.
func (h *Host) classifies(name string) bool {
	t, _ := h.target(name)
	return t != nil && t.Classify
}

// WarnModel toasts the model row's warning after a reload, if any: a
// config the provider does not list would otherwise run silently until the
// provider refuses it.
func (h *Host) WarnModel() {
	if row := h.modelRow(); row.Level != HealthOK {
		title := _unlistedTitle
		if row.Level == HealthError {
			title = "Model unavailable"
		}
		h.Notify(slog.LevelWarn, title, row.Detail)
	}
}

// OfferModel opens the model picker when no model can run the next send,
// so a first run starts on the keys to export rather than a failed send. A
// failed init.lua is its own news: the providers it adds never ran.
func (h *Host) OfferModel() {
	if h.InitError() == "" && h.modelRow().Level == HealthError {
		_ = h.OpenModelPicker("")
	}
}

// setWatched writes a config key from any goroutine: the watches it fires
// run later on the interpreter queue. It checks only the kind: SetConfig
// sends a key with a setter of its own there first.
func (h *Host) setWatched(path config.Key, val any) error {
	fired, err := h.writeSetting(path, val)
	if err != nil {
		return err
	}
	if len(fired) > 0 {
		h.GoSafe("config watches", func() {
			_, _ = h.RT.Do(context.Background(), lua.ClassUI, 0, 0, "config watch", func(L *glua.LState) ([]any, error) {
				h.deliverFired(L, fired)
				return nil, nil
			})
		})
	}
	h.refresh()
	return nil
}

// plainTextEnd returns the final plain-text reply of an idle run, or "" when
// it ended in a tool call (an approved or dismissed plan).
func plainTextEnd(runner *agent.Runner) string {
	h := runner.Snapshot()
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

// OpenEffortPicker lists the reasoning levels and the model's own, each with
// what it sends on the conversation's model; enter sets the pick.
func (h *Host) OpenEffortPicker() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	pick, model := h.Settings.Str(config.KeyAgentEffort), h.AgentStream().Model
	items := make([]OverlayItem, 0, len(agent.Efforts)+1)
	sel := 0
	for _, level := range append(strs(agent.Efforts), "") {
		name := cmp.Or(level, "default")
		item := OverlayItem{Text: name, Run: "effort " + name, Desc: h.effortDesc(model, level)}
		if level == pick {
			item.Tag, sel = "current", len(items)
		}
		items = append(items, item)
	}
	ui.OpenOverlay(Overlay{ID: "effort", Title: "Effort", Origin: lua.OriginBuiltin, Items: items, Selected: sel, Anchor: OverlayBottomRight, Verb: "pick"})
	return nil
}

// effortDesc is what level sends on model: the pick itself, the level a
// catalog model lists nearest it, or none at all.
func (h *Host) effortDesc(model, level string) string {
	if model == "" {
		return ""
	}
	sent := h.Providers.Effort(agent.ModelID(model), agent.Effort(level))
	switch {
	case sent == "":
		return "the model takes no reasoning effort"
	case level == "":
		return "the model's own: " + sent
	default:
		return "sent as " + sent
	}
}

// slotLine is what a model row shows: the model the slot runs on and the
// description beside it.
func (h *Host) slotLine(name, desc string) string {
	return cmp.Or(h.roleModel(name), "no model") + "   " + desc
}

func (h *Host) agentNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("current", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		ag := h.agents
		ag.mu.Lock()
		model := h.liveModelID()
		ag.mu.Unlock()
		out.RawSetString("model", glua.LString(model))
		L.Push(out)
		return 1
	}))

	// prompt() is the pinned system prompt and context message the last send
	// carried, "" before the first one; it is never rebuilt here (see
	// SystemPrompt).
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
	// the user's words (DESIGN §6.3).
	tb.RawSetString("send", L.NewFunction(func(L *glua.LState) int {
		text := L.CheckString(1)
		principal, body := "", ""
		if h.RT.CurrentOrigin(L) == lua.OriginPlugin {
			if h.hooks() == nil { // zemi health boots plugins with no UI: a turn nobody sees has nothing to bound it
				L.RaiseError("zemi.agent.send: no UI is running to send from")
			}
			if stub := h.luaGate(L, _luaAgentSend, "*", func() map[string]any { return map[string]any{"text": text} }); stub != nil {
				L.Push(glua.LTrue) // a test zemi's stub keeps the text and starts no turn
				return 1
			}
			principal = h.principal(L)
			body = text
			text = agent.Attached(principal, text)
		}
		if _, err := h.resolveModel(h.roleModel(_defaultRole)); err != nil {
			return luaResult(L, err)
		}
		h.GoSafe("zemi.agent.send", func() {
			// the plugin's message draws its row where it was sent, the same
			// one a rebuilt window reads from the log
			if principal != "" {
				h.work("", principal, "", "", body)
			}
			if err := h.AgentSend(text); err != nil {
				h.Log.Warn("zemi.agent.send failed", "origin", "agent", "error", err)
				h.Notify(slog.LevelError, "Could not send", h.friendlyError(err))
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
		out := usageTable(L, s, h.modelPriced(h.turnModelID()))
		out.RawSetString("context", contextTable(L, s))
		L.Push(out)
		return 1
	}))

	h.agentRunFns(L, tb)
	return tb
}

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
		ag.reads = map[string]*sync.Map{sid: {}}
		ag.instr = set.Set[string]{}
		ag.loaded = &sync.Map{}
	}
	return ag.reads[sid], ag.loaded
}

func (h *Host) seedReadsInto(seen *sync.Map, rows []*store.Message) {
	touched := map[string]int64{} // path -> the time of the row that last read or wrote it
	for _, row := range rows {
		var m fantasy.Message
		if json.Unmarshal([]byte(row.PartsJSON), &m) != nil {
			continue
		}
		for _, part := range m.Content {
			call, ok := part.(fantasy.ToolCallPart)
			if !ok || !slices.Contains([]string{string(tools.ToolRead), string(tools.ToolWrite), string(tools.ToolEdit)}, call.ToolName) {
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

// pinFiles are the files the pinned prompt carries: a run whose reads are its
// own still sees them as given.
func (h *Host) pinFiles() []string {
	h.agents.mu.Lock()
	defer h.agents.mu.Unlock()
	return slices.Collect(maps.Keys(h.agents.pin.parts.files))
}

// mainInstructions is the main agent's Instructions hook: the files the pin read.
func (h *Host) mainInstructions(path string) string {
	return h.nearestInstructions(path, h.pinFiles(), h.instructionSent)
}

// afterWrite is the pipeline's AfterWrite hook; the recipe check may rewrite the file.
func (h *Host) afterWrite(path string) string {
	return h.recipeAfterWrite(path) + h.lspAfterWrite(path)
}

// PipelineFor builds the tools pipeline for the model's tool calls. It is
// built per send (mainTools -> ensureRunner), so the pipeline's settings
// match the runner the send built. The pad is the live session's, bound as
// the prompt names it.
func (h *Host) PipelineFor() *tools.Pipeline {
	p := &tools.Pipeline{
		Pre:        h.toolPreHook(),
		Ask:        h.askHook(),
		Veto:       h.vetoHook(),
		Log:        h.Log,
		AfterWrite: h.afterWrite,
		Changes:    h.shellChanges,
		Progress: func(id string, p tools.Progress) {
			h.setToolProgress(id, sanitize.String(p.Line), sanitize.String(p.Tail))
		},
		Outcome: h.setToolOutcome,
		Denied: func(tool string, req tools.Request, v tools.Verdict) {
			h.consultHooks(EventToolDenied, map[string]any{"tool_name": tool, "tool_input": req.Preview, "arg": req.Arg, "by": v.DecidedBy, "reason": v.Reason})
			h.EmitAsync(EventToolDenied, map[string]any{"tool": tool, "subject": req.Subject, "arg": req.Arg, "by": v.DecidedBy, "reason": v.Reason})
		},
		Post:   h.toolPost,
		Jobs:   h.jobs,
		Skills: h.skillIndex,
		Images: func() bool {
			h.agents.mu.Lock()
			id := h.liveModelID()
			h.agents.mu.Unlock()
			return h.takesImages(id)
		},
		Audio: func() bool {
			h.agents.mu.Lock()
			id := h.liveModelID()
			h.agents.mu.Unlock()
			return h.takesAudio(id)
		},
		Pad:          h.ScratchDir(),
		ProjectDir:   h.Dirs.ProjectDir,
		Seen:         h.readLog(),
		Instructions: h.mainInstructions,
	}
	if ui := h.hooks(); ui != nil {
		p.Question = func(qs []tools.Question) ([]tools.Answer, bool) {
			done := h.holdPrompt()
			defer done()
			return ui.AskQuestion(qs)
		}
		p.ApprovePlan = h.approvePlan
	}
	if h.DB != nil {
		p.TodosRead = func() ([]store.Todo, error) { return h.DB.Todos(store.MainSessionID) }
		p.TodosWrite = func(todos []store.Todo) error {
			defer h.todosStale.Store(true)
			if err := h.DB.SetTodos(store.MainSessionID, todos); err != nil {
				return err
			}
			h.resetTodoNag(todos)
			return nil
		}
	}
	return p
}
