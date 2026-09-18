// agent.go is the agent core in the host (DESIGN §6.3): the
// provider/model/agent/permission/session namespaces, profile resolution,
// turn streaming into the UI, message persistence and the steering queue.
package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
type Profile struct {
	Name         string   `json:"name"`
	Model        string   `json:"model"`
	System       string   `json:"system"`
	Tools        []string `json:"tools"`
	ContextFiles []string `json:"context_files"`
	Temperature  *float64 `json:"temperature,omitempty"`
}

// PermRequest is one interactive permission ask for the overlay.
type PermRequest struct {
	Tool    string // the asking tool (bash, edit, mcp_github_search)
	Always  string // what the session-long choice allows ("allow `go test`"), "" when none is offered
	Subject string
	Arg     string
	Reason  string
	Preview string // file tools: the proposed change as "- "/"+ " lines
	// Description is the model's summary of a command, not checked against it
	Description string
}

// StreamTool is one tool call in the live stream view (DESIGN §6.3).
type StreamTool struct {
	ID      string
	Label   string // the model's short description of a command
	Started time.Time
	Detail  string // live progress line (subagents, workflows)
	Run     string // agent tool: the run id, so the row can open its transcript
	Diff    string // file tools: the change as "- "/"+ " lines
	Output  string // the last lines of the result (shortOutput)
	Name    string
	Input   string
	Status  string // "run", "ok", "err"
}

// agentHost owns the live agent session: profiles, the runner, the
// steering queue and the stream view the UI renders.
type agentHost struct {
	mu        sync.Mutex
	profiles  map[string]*Profile
	defProf   string
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
	sessIn   int64
	sessOut  int64
	lastIn   int64
	ctxWin   int64
	sessCost float64 // USD, from model spec prices

	// readMu guards reads and instr, the per-session file bookkeeping the
	// tools pipeline carries (what the model has read, which instruction
	// files it has been given). Both are dropped when the session changes.
	readMu sync.Mutex
	reads  map[string]*sync.Map
	instr  map[string]bool

	// promptTokens estimates the system prompt and tool schemas the runner
	// carries; every prompt holds them, so the context estimate counts them.
	promptTokens int64

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
	holdImages   bool // a 400 over image parts is resent without them, not reported
	holdCap      bool // a refused output cap is resent without it, not reported

	// digest is pinned at session start (DESIGN §10.7) and refreshed only
	// at compaction; memAuto counts auto-applied memories against the
	// session cap (memory proposals persist in the store).
	digest     string
	memAuto    int
	reflectErr string // last reflector run's error, "" after a success (/health)

	// turnMsgID is the stored user message of the running turn; write tools
	// checkpoint files against it (rewind).
	turnMsgID string

	// shellNotes holds !command output waiting for the next message.
	shellNotes []string

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
func (h *Host) Effort() string { return h.setStr("agent.effort") }

// setStr reads a string setting, "" when unset or not a string.
func (h *Host) setStr(key string) string {
	v, _ := h.Settings.Get(key)
	s, _ := v.(string)
	return s
}

// setInt reads a number setting, def when unset or not a number (config
// files decode numbers as float64).
func (h *Host) setInt(key string, def int) int {
	v, _ := h.Settings.Get(key)
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return def
}

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
	if s := h.setStr("agent.model"); s != "" {
		return s
	}
	for _, spec := range h.Providers.List() {
		for _, m := range spec.Models {
			return spec.ID + "/" + m.ID
		}
	}
	return ""
}

// liveModelID is the model turns run on: the running turn's, else the
// default profile's (ag.mu held).
func (h *Host) liveModelID() string {
	ag := h.agents
	if ag.active && ag.turnModel != "" {
		return ag.turnModel
	}
	return h.currentModelID(ag.profiles[ag.defProf])
}

// sessionModelID is liveModelID for callers not holding ag.mu.
func (h *Host) sessionModelID() string {
	h.agents.mu.Lock()
	defer h.agents.mu.Unlock()
	return h.liveModelID()
}

func (h *Host) builtinTools(pipe *tools.Pipeline) []fantasy.AgentTool {
	return append(tools.All(pipe), h.agentTool(), h.workflowTool(), h.luaEvalTool(), h.windowTool())
}

// headlessExcluded needs a user at a terminal: with no UI hooks these tools
// can only refuse, so a print run does not pay for their schemas every step.
var headlessExcluded = map[string]bool{"window": true, "question": true, "plan": true, "open_in_editor": true}

// profileTools selects the tool set: profile names filter the builtin set;
// Lua-registered tools are always included (the pipeline gates them).
func (h *Host) profileTools(p *Profile) []fantasy.AgentTool {
	var want []string
	if p != nil && len(p.Tools) > 0 {
		want = p.Tools
	}
	var skip map[string]bool
	if h.hooks() == nil {
		skip = headlessExcluded
	}
	pipe := h.PipelineFor()
	out, _ := filterTools(h.builtinTools(pipe), want, skip)
	out = append(out, h.luaFantasyTools(pipe)...)
	return append(out, h.mcpFantasyTools(pipe)...)
}

func (h *Host) ensureRunner(p *Profile) (*agent.Runner, error) {
	model, err := h.resolveModel(p)
	if err != nil {
		return nil, err
	}
	toolset, sys := h.profileTools(p), h.buildSystem(p)
	r := agent.NewRunner(model, toolset, sys)
	h.agents.mu.Lock()
	h.agents.promptTokens = promptTokens(sys, toolset)
	h.agents.mu.Unlock()
	r.ProviderOptions = h.Providers.EffortOptions(h.currentModelID(p), h.Effort())
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

// promptTokens estimates what every prompt carries besides the messages:
// the system prompt and the tool schemas sent with it.
func promptTokens(sys string, toolset []fantasy.AgentTool) int64 {
	n := len(sys)
	for _, t := range toolset {
		info := t.Info()
		schema, _ := json.Marshal(info.Parameters)
		n += len(info.Name) + len(info.Description) + len(schema)
	}
	return int64(n / 4)
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

// sendClaimed starts a turn for text on an agent already claimed with scope.
func (h *Host) sendClaimed(scope *turnScope, text string) error {
	ag := h.agents
	fail := func(err error) error {
		h.endScope(scope)
		h.refresh()
		return err
	}
	ag.mu.Lock()
	profName := ag.defProf
	ag.mu.Unlock()

	p, err := h.resolveProfile(profName)
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

	h.GoSafe("agent turn", func() { h.runTurns(runner, scope, text) })
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
	shown, _, _ := strings.Cut(text, attachMarker)
	h.AppendMessage("user", shown)
	h.agentStream()
	h.EmitAsync("agent.start", map[string]any{"text": text})
}

// nextTurn runs after each turn: messages queued after the last step
// boundary become one follow-up turn, otherwise the agent goes idle.
// active stays held across the handoff, so a submit in between queues
// instead of starting a turn out of order. After a cancelled turn nothing
// starts: the queue waits for the UI to hand it back to the composer.
func (h *Host) nextTurn(done *turnScope) (*turnScope, string) {
	ag := h.agents
	ag.mu.Lock()
	text := ""
	if done.ctx.Err() == nil {
		text = joinQueue(ag.steering)
		ag.steering = nil
	}
	var next *turnScope
	if text == "" {
		ag.active = false
		h.turn.Store(nil)
	} else {
		ag.beginTurn()
		next = h.beginScope()
	}
	ag.mu.Unlock()
	done.cancel()
	if text == "" {
		h.refresh()
	} else {
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
		out[i] = m // Runner.Compose expands it
	}
	return out
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
	// fantasy passes no error when a network failure (DNS, refused, reset)
	// caused the retry.
	ag.retryWhy = "network problem"
	if err != nil {
		ag.retryWhy = friendlyError(err)
	}
	ag.mu.Unlock()
	s.h.Log.Warn("agent", "provider retry", map[string]any{"error": errString(err), "delay_ms": delay.Milliseconds()})
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

func (s *streamAdapter) ToolCallStart(id, name, input string) {
	// the diff reads the file: outside the lock, which UI readers share
	diff := ""
	if name == "write" || name == "edit" || name == "multiedit" {
		var in map[string]any
		if json.Unmarshal([]byte(input), &in) == nil {
			diff = capLines(tools.ChangeDiff(name, in), 24)
		}
	}
	ag := s.h.agents
	ag.mu.Lock()
	ag.endThinking()
	// Text streamed before this call belongs above it: move it and the
	// earlier (finished) tool rows into the transcript, in order, so the
	// next step's text starts its own block instead of running on.
	reason, tools, text := ag.reasonBuf, ag.tools, ag.textBuf
	stat := TurnStat{ThoughtMS: ag.thought.Milliseconds()}
	flush := strings.TrimSpace(text) != ""
	if flush {
		ag.reasonBuf, ag.tools, ag.textBuf, ag.thought = "", nil, "", 0
	}
	ag.tools = append(ag.tools, StreamTool{ID: id, Name: name, Input: toolSummary(input), Label: toolLabel(input), Started: time.Now(), Status: "run", Diff: diff})
	ag.mu.Unlock()
	if flush {
		s.h.appendTurnOutput(reason, stat, tools, text, TurnStat{})
	}
	s.h.agentStream()
}

func (s *streamAdapter) ToolCallEnd(id, name string, ok bool, output string) {
	ag := s.h.agents
	ag.mu.Lock()
	for i := len(ag.tools) - 1; i >= 0; i-- {
		if ag.tools[i].ID == id && ag.tools[i].Status == "run" {
			ag.tools[i].Status = map[bool]string{true: "ok", false: "err"}[ok]
			ag.tools[i].Output = shortOutput(output)
			if !ok {
				ag.tools[i].Diff = "" // the change did not happen
			}
			break
		}
	}
	ag.mu.Unlock()
	s.h.agentStream()
	s.h.recordToolCall(s.h.AgentSessionID(), name, ok)
}

func (s *streamAdapter) TurnDone(err error, usage fantasy.Usage) {
	h := s.h
	ag := h.agents
	ag.mu.Lock()
	// sendCompacting resends a held error or reports it; the steps before it
	// still ran, so their usage and rows are kept
	hold := ag.holdOverflow && contextTooLarge(err) || ag.holdImages && imagesRefused(err) || ag.holdCap && maxTokensRefused(err)
	ag.holdOverflow, ag.holdImages, ag.holdCap = false, false, false
	model := ag.turnModel
	ag.mu.Unlock()
	if model == "" {
		model = h.currentModelID(nil) // a turn the host did not start (tests)
	}
	cost := h.turnCost(model, usage)
	ag.mu.Lock()
	ag.endThinking()
	ag.sessCost += cost
	ag.sessIn += billedInput(usage)
	ag.sessOut += usage.OutputTokens
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
	if sid != "" && h.DB != nil {
		_ = h.DB.AddUsage(sid, billedInput(usage), usage.OutputTokens, cost)
	}
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
		row := ChatMessage{Role: "assistant", Kind: "error", Text: truncRunes(sanitize.String(errorRowText(err)), 400)}
		if errors.Is(err, context.Canceled) {
			row.Status = "cancelled"
		}
		h.appendChatMessage(row)
	} else {
		h.EmitAsync("agent.finish", map[string]any{})
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
		}
		if status != "ok" {
			t.Diff = ""
		}
		h.appendChatMessage(ChatMessage{Role: "assistant", Kind: "tool", Tool: t.Name, Text: t.Input, Label: t.Label, Status: status, Detail: t.Detail, Run: t.Run, Diff: t.Diff, Output: t.Output})
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

// toolLabel is the description a model gives a command (bash, job_start),
// shown in place of the command; "" for other tools.
func toolLabel(input string) string {
	var in struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	if json.Unmarshal([]byte(input), &in) != nil || in.Command == "" {
		return ""
	}
	return learn.Truncate(strings.Join(strings.Fields(in.Description), " "), 80)
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
		// the plan itself shows as a message above the row: name its first line
		for line := range strings.Lines(plan) {
			if line = strings.TrimSpace(strings.TrimLeft(line, "# \t")); line != "" {
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
	// the window and lsp tools name what they do in "action"; without it a
	// window row would read as the whole panel body it is about to show
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
// real FK). The output is not kept: the history row already holds it.
func (h *Host) recordToolCall(sid, name string, ok bool) {
	if h.DB == nil || sid == "" {
		return
	}
	parts, _ := json.Marshal(map[string]any{"tool": name})
	msg := &store.Message{SessionID: sid, Role: "tool", PartsJSON: string(parts)}
	tc := &store.ToolCall{Tool: name, InputJSON: "{}", OK: ok}
	if !ok {
		tc.ErrorKind = "error"
	}
	if err := h.DB.AddToolCall(msg, tc); err != nil {
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
	ag.digest = h.digestFor(firstText)
	ag.env = ""
	ag.memAuto = 0
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
		Profile: ag.defProf,
		Thought: thought, Elapsed: elapsed,
		SessionIn: ag.sessIn, SessionOut: ag.sessOut, LastIn: ag.lastIn,
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
		if h.RT.CurrentOrigin(L) != lua.OriginUser {
			L.RaiseError("zemi.provider.register: only user config may register providers")
			return 0
		}
		spec, err := providerSpecFromLua(L, 1)
		if err != nil {
			L.RaiseError("zemi.provider.register: %v", err)
			return 0
		}
		if err := h.Providers.Register(spec, rawString(L.CheckTable(1), "api_key")); err != nil {
			L.RaiseError("zemi.provider.register: %v", err)
			return 0
		}
		return 0
	}))
	kinds := L.NewTable()
	kinds.RawSetString("ANTHROPIC", glua.LString(agent.KindAnthropic))
	kinds.RawSetString("OPENAI", glua.LString(agent.KindOpenAI))
	kinds.RawSetString("OPENAI_COMPAT", glua.LString(agent.KindOpenAICompat))
	tb.RawSetString("kind", kinds)
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
		return agent.Spec{}, errors.New("spec must be a table")
	}
	if name := rawString(tb, "api_key_env"); name != "" {
		return agent.Spec{}, fmt.Errorf(`api_key_env is gone: pass the key itself, api_key = zemi.env.get("%s")`, name)
	}
	spec := agent.Spec{
		ID:      rawString(tb, "id"),
		Kind:    rawString(tb, "kind"),
		BaseURL: rawString(tb, "base_url"),
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
		if _, ok := h.modelSpec(id); !ok {
			L.RaiseError("zemi.model.set: unknown model %q", id)
			return 0
		}
		fired, err := h.Settings.SetQuiet("agent.model", id)
		if err != nil {
			L.RaiseError("zemi.model.set: %v", err)
			return 0
		}
		h.deliverFired(L, fired)
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
	cur, sel := h.setStr("agent.model"), 0
	var items []any
	for _, s := range h.Providers.List() {
		for _, m := range s.Models {
			id := s.ID + "/" + m.ID
			item := map[string]any{"text": id, "run": "model " + id}
			if m.Display != id {
				item["desc"] = m.Display
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
	ui.OpenOverlay(Overlay{ID: "model", Title: "Model", Origin: lua.OriginBuiltin, Items: items, Selected: sel, Empty: "No models yet. Add a provider in init.lua."})
	return nil
}

// SetModel switches agent.model (/model <id>) from any goroutine: the
// config watches it fires run later on the interpreter queue.
func (h *Host) SetModel(id string) error {
	if _, ok := h.modelSpec(id); !ok {
		return fmt.Errorf("unknown model %q", id)
	}
	fired, err := h.Settings.SetQuiet("agent.model", id)
	if err != nil {
		return err
	}
	if len(fired) > 0 {
		h.GoSafe("model watches", func() {
			_, _ = h.RT.Do(context.Background(), lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
				h.deliverFired(L, fired)
				return nil, nil
			})
		})
	}
	h.refresh()
	return nil
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
		p := &Profile{Name: name, Model: rawString(spec, "model"), System: rawString(spec, "system")}
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

	tb.RawSetString("set_default", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		h.refuseBelowUser(L, "agent.set_default")
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
		sid, model := ag.sessionID, h.liveModelID()
		ag.mu.Unlock()
		out.RawSetString("id", glua.LString(sid))
		out.RawSetString("model", glua.LString(model))
		L.Push(out)
		return 1
	}))

	// send returns at once: starting a turn builds the system prompt, which
	// runs agent.assemble handlers on this interpreter and must not wait on it.
	tb.RawSetString("send", L.NewFunction(func(L *glua.LState) int {
		text := L.CheckString(1)
		h.refuseBelowUser(L, "agent.send")
		h.GoSafe("zemi.agent.send", func() {
			if err := h.AgentSend(text); err != nil {
				h.Log.Warn("agent", "zemi.agent.send failed", map[string]any{"error": err.Error()})
				h.Notify("error", "Could not send", err.Error())
			}
		})
		return 0
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
		}
		switch rawString(spec, "scope") {
		case "", "session":
		case "permanent":
			L.RaiseError(`zemi.permission.rule: scope "permanent" was removed; a rule in init.lua applies on every start, so drop the scope field`)
		default:
			L.RaiseError(`zemi.permission.rule: scope must be "session"`)
		}
		// Origins (DESIGN §5, §6.1): allow rules widen, so they come from
		// user config only; every other origin may only add deny/ask rules.
		if h.RT.CurrentOrigin(L) != lua.OriginUser && r.Decision == perm.Allow {
			L.RaiseError("zemi.permission.rule: allow rules may only come from user config")
			return 0
		}
		if err := validateRule(&r, rawString(spec, "match"), rawString(spec, "not_match_example")); err != nil {
			L.RaiseError("zemi.permission.rule: %v", err)
			return 0
		}
		if h.Perm.Add(r) || h.claimStale(r) {
			// only rules this call added: an equal one answered "always"
			// must survive a reload
			h.luaRulesMu.Lock()
			h.luaRules = append(h.luaRules, r)
			h.luaRulesMu.Unlock()
		}
		return 0
	}))

	tb.RawSetString("mode", h.modeNamespace(L))

	tb.RawSetString("rules", L.NewFunction(func(L *glua.LState) int {
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

	return tb
}

// validateRule checks a Lua rule. Examples are required (DESIGN §9): the match example must match
// the pattern without being excluded, the not_match example must be
// excluded; a guard needs both examples.
func validateRule(r *perm.Rule, matchEx, notMatchEx string) error {
	switch {
	case r.Subject == "" || r.Pattern == "":
		return errors.New("subject and pattern are required")
	case r.Decision != perm.Allow && r.Decision != perm.Ask && r.Decision != perm.Deny:
		return errors.New("decision must be allow, ask or deny")
	}
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

// toolPostHook emits tool.post after every builtin tool run.
func (h *Host) toolPostHook() func(string, map[string]any, bool, string) {
	return func(name string, input map[string]any, ok bool, output string) {
		h.EmitAsync("tool.post", map[string]any{
			"tool": name, "input": input, "ok": ok,
			"output": truncRunes(output, 4096),
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

// PipelineFor builds the tools pipeline for the model's tool calls.
func (h *Host) PipelineFor() *tools.Pipeline {
	p := &tools.Pipeline{
		Store:       h.Perm,
		Pre:         h.toolPreHook(),
		Ask:         h.askHook(),
		Checkpoint:  h.checkpointFile,
		AfterWrite:  h.lspAfterWrite,
		Diagnostics: h.lspDiagnostics,
		LSP:         h.lspOpen,
		Plan:        func() bool { return h.permModeDef().Base == "plan" },
		Sandbox: func() *sandbox.Policy {
			// yolo allows everything anyway, so confining it only breaks commands
			if def := h.permModeDef(); h.sandboxLive() && def.Base != "yolo" {
				p := h.sandboxPolicy(def.Base == "plan")
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
		Remember: func(scope, kind, text string, tags []string) (bool, error) {
			stored, _, err := h.proposeMemory(h.AgentSessionID(), learn.OriginLearned, scope, kind, text, tags)
			return stored, err
		},
		Recall: func(query string, limit int) ([]*store.Memory, error) {
			return h.memorySearch("all", query, limit, true)
		},
		EditMemory:   func(id, text string) (string, error) { return h.modelEditMemory(h.AgentSessionID(), id, text) },
		Scratch:      h.inScratch,
		Protected:    func(p string) bool { return !h.inScratch(p) && h.protectedWrite(p) },
		ScratchDir:   h.ScratchDir,
		SearchURL:    func() string { return h.setStr("web.search_url") },
		Seen:         h.readLog(),
		Instructions: func(path string) string { return h.nearestInstructions(path, h.instructionSent) },
	}
	if ui := h.hooks(); ui != nil {
		p.Question = ui.AskQuestion
		p.ApprovePlan = h.approvePlan
		p.Edit = func(ctx context.Context, path string, line int, title string) error {
			err := ui.EditFile(ctx, EditRequest{Path: path, Line: line, Title: title})
			return err
		}
	}
	if h.DB != nil {
		p.TodosRead = func() ([]store.Todo, error) { return h.DB.Todos(h.AgentSessionID()) }
		p.TodosWrite = func(todos []store.Todo) error {
			sid := h.AgentSessionID()
			if sid == "" {
				return errors.New("no active session")
			}
			defer h.todosStale.Store(true)
			return h.DB.SetTodos(sid, todos)
		}
	}
	return p
}
