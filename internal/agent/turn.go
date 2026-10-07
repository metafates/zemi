package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"

	"github.com/metafates/zemi/internal/tools"
)

// StreamHooks receives streaming updates; implementations must be safe
// to call from the streaming goroutine.
type StreamHooks interface {
	TextDelta(text string)
	ReasoningDelta(text string)
	// ToolCallPending opens a call's row as the model starts streaming its
	// arguments; ToolCallStart then fills that row in. A provider that
	// reports no input start only calls ToolCallStart.
	ToolCallPending(id, name string)
	// ToolInputDelta is a piece of the pending call's JSON arguments.
	ToolInputDelta(id, delta string)
	ToolCallStart(id, name, input string)
	// ToolCallRunning is when the call starts to run: a serial call (shell)
	// holds every later call of its step until it returns.
	ToolCallRunning(id string)
	ToolCallEnd(id, name string, result fantasy.ToolResultOutputContent, diff string)
	// StepStart marks the start of a model call; after Retrying the call
	// streams again from its first delta.
	StepStart()
	// StreamDone is a model call's usage as its stream finishes, before its
	// tools run: the one place a turn's spend is counted, so a tally moves
	// while a slow tool runs.
	StreamDone(u fantasy.Usage)
	// StepDone reports a completed step, its tools run.
	StepDone(s Step)
	// Retrying reports a provider retry (rate limit, overload) before the
	// backoff delay.
	Retrying(err error, delay time.Duration)
	TurnDone(err error, usage fantasy.Usage)
}

// Step is one finished model call of a turn.
type Step struct {
	N     int // 1-based within the send
	Usage fantasy.Usage
	// Took runs from the call's start to its end; FirstToken to the first
	// text, reasoning or tool argument it streamed, 0 when none came
	Took, FirstToken time.Duration
	Tools            int // tool calls it made
}

// Runner runs one conversation: history in, streaming turns out. Send is
// safe for one concurrent turn; a second Send while one runs returns
// ErrTurnActive (mid-turn messages go through Steer instead, DESIGN §4).
type Runner struct {
	mu      sync.Mutex
	agent   fantasy.Agent
	last    *LastErr
	History []fantasy.Message
	Hooks   StreamHooks
	// Persist receives every message of the turn, user first, then the
	// step messages, as each happens
	Persist     func(msg fantasy.Message) error
	Temperature *float64

	// MaxOutputTokens caps each reply; 0 leaves the provider default.
	MaxOutputTokens int64

	// MaxSteps caps the steps one turn may take before it stops and hands
	// back; 0 leaves the default.
	MaxSteps int

	// Context is a user message every request sends right after the system
	// prompt and before the history: what varies by session or project, so
	// that the system prompt and the tools are one cached prefix for all of
	// a user's sessions. It never joins History.
	Context string

	// ProviderOptions ride on every call (reasoning effort).
	ProviderOptions fantasy.ProviderOptions
	// Headers ride every call (SessionHeaders).
	Headers map[string]string

	// Steer is polled at every step boundary; the messages it returns join
	// the running turn as user messages (DESIGN §4 steering).
	Steer func() []string

	// ActiveTools names the tools a step offers, asked before every step; nil
	// offers them all. Every tool stays registered, so a call still runs.
	ActiveTools func() []string

	// Compose builds the user message for a sent or steered text (the host
	// expands attachments and images); nil sends the text as is.
	Compose func(text string) fantasy.Message

	// TextOnly strips image parts from the history before every send: a
	// user attachment becomes a note, an image tool result a text error.
	TextOnly bool

	// Full is asked after every step with the context that step used
	// (ContextUsed). True while the model still has tool results to read
	// ends the turn there, so the host can compact and send "" to continue.
	Full func(used int64) bool

	active bool
	cut    bool
	cancel context.CancelFunc
	tools  []fantasy.AgentTool
	// system is the prompt this runner was built with: a fork continues on the
	// same one, and rebuilding that text would send a different prefix
	system string
	// live is the running step's input, which holds the turn's finished steps
	live []fantasy.Message
	// sent and key are the messages and the prefix key the last step sent
	// (signed), kept across turns and carried by Continue
	sent []fantasy.Message
	key  string
	// offered is the tool names the last request offered, and carried the
	// caller's own, which a fork's first request offers instead of its own set:
	// another tools array is a prefix the provider has not cached
	offered []string
	carried []string
}

// Continue carries prev's conversation into r, a runner built for the next
// send, with what prev last sent so signed still knows which thinking holds.
func (r *Runner) Continue(prev *Runner) {
	prev.mu.Lock()
	defer prev.mu.Unlock()
	r.History, r.sent, r.key = prev.History, prev.sent, prev.key
}

// CutShort reports whether the last Send ended because Full said the
// context is full.
func (r *Runner) CutShort() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cut
}

// ErrTurnActive is returned when a turn is already running.
var ErrTurnActive = errors.New("agent: a turn is already active")

// AttachMarker starts what zemi appends to a user message for the model:
// attachments, shell output, agent reports and notes. Everything that shows,
// stores or recalls the user's words cuts the text there.
const AttachMarker = "\n\n<attached-files>"

// Attached puts text the user did not type (piped stdin, a command file's
// body, a note from zemi) behind AttachMarker as one element, so it never
// reads as the user's words or as unlabeled attachment text.
func Attached(tag, text string) string {
	return AttachMarker + "\n<" + tag + ">\n" + text + "\n</" + tag + ">\n</attached-files>"
}

// CutAttached splits a message into what was typed and its trailer.
func CutAttached(s string) (typed, trailer string) {
	if i := strings.Index(s, AttachMarker); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

// Note wraps text zemi itself sends the model (a nag, a plan handoff).
func Note(text string) string { return Attached("zemi-note", text) }

// ErrReplyCut ends a turn whose last reply hit the output token limit.
var ErrReplyCut = errors.New("the reply hit the output token limit (the model's max_output) and was cut off")

// NewRunner builds a runner over a resolved model.
func NewRunner(model fantasy.LanguageModel, tools []fantasy.AgentTool, system string) *Runner {
	r := &Runner{last: &LastErr{LanguageModel: model}, system: system}
	opts := []fantasy.AgentOption{}
	if system != "" {
		opts = append(opts, fantasy.WithSystemPrompt(system))
	}
	if len(tools) > 0 {
		r.tools = make([]fantasy.AgentTool, len(tools))
		for i, t := range tools {
			r.tools[i] = runningTool{t, r, i == len(tools)-1}
		}
		opts = append(opts, fantasy.WithTools(r.tools...))
	}
	r.agent = fantasy.NewAgent(r.last, opts...)
	return r
}

// runningTool reports when fantasy starts a call, which is later than its
// arguments end when a serial call ahead of it still runs.
type runningTool struct {
	fantasy.AgentTool
	r    *Runner
	last bool
}

// ProviderOptions puts the fourth cache breakpoint on the last tool, so a
// new system prompt over the same tools still reads their schemas from the
// cache. NOTE: a step whose ActiveTools leave the last tool out has none.
func (t runningTool) ProviderOptions() fantasy.ProviderOptions {
	if t.last {
		return withCacheControl(t.AgentTool.ProviderOptions())
	}
	return t.AgentTool.ProviderOptions()
}

func (t runningTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	t.r.mu.Lock()
	hooks := t.r.Hooks
	t.r.mu.Unlock()
	if hooks != nil {
		hooks.ToolCallRunning(call.ID)
	}
	return t.AgentTool.Run(ctx, call)
}

// LastErr remembers a model call's failure: fantasy's retry callback gets
// only a *ProviderError, nil when a network failure caused the retry.
type LastErr struct {
	fantasy.LanguageModel
	Err error
}

func (m *LastErr) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	res, err := m.LanguageModel.Generate(ctx, call)
	m.Err = err
	return res, err
}

func (m *LastErr) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	seq, err := m.LanguageModel.Stream(ctx, call)
	m.Err = err
	if err != nil {
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		for p := range seq {
			if p.Type == fantasy.StreamPartTypeError {
				m.Err = p.Error
			}
			if !yield(p) {
				return
			}
		}
	}, nil
}

// Cancel aborts the active turn, if any.
func (r *Runner) Cancel() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
}

// Send streams one turn. The user message is appended to history,
// persisted, and the agent streams; step messages are appended and
// persisted as they complete. An empty userText continues from history as
// it stands (a turn Full cut short).
func (r *Runner) Send(ctx context.Context, userText string) error {
	r.mu.Lock()
	if r.active {
		r.mu.Unlock()
		return ErrTurnActive
	}
	r.active, r.cut, r.live = true, false, nil
	tctx, cancel := context.WithCancel(tools.CallScope(ctx))
	r.cancel = cancel
	hooks := r.Hooks
	persist := r.Persist
	full := r.Full
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.active = false
		r.cancel = nil
		r.mu.Unlock()
		cancel()
	}()

	var userMsg []fantasy.Message
	if userText != "" {
		msg := r.compose(userText)
		if persist != nil {
			if err := persist(msg); err != nil {
				hooks.TurnDone(err, fantasy.Usage{})
				return err
			}
		}
		userMsg = append(userMsg, msg)
	}
	r.mu.Lock()
	r.History = append(closeToolCalls(r.History), userMsg...)
	if r.TextOnly {
		r.History = stripImages(r.History, noImages)
	}
	history := slices.Clone(r.History)
	r.mu.Unlock()

	// Full goes first: fantasy stops at the first condition that holds, and
	// Full has to see every step.
	var stops []fantasy.StopCondition
	cut := false
	if full != nil {
		stops = append(stops, func(steps []fantasy.StepResult) bool {
			last := steps[len(steps)-1]
			// a tool that ends the turn (a plan or question answered) is not cut
			stop := full(ContextUsed(last.Usage)) && last.FinishReason == fantasy.FinishReasonToolCalls &&
				!slices.ContainsFunc(last.Content.ToolResults(), func(r fantasy.ToolResultContent) bool { return r.StopTurn })
			cut = cut || stop
			return stop
		})
	}
	// A turn stuck repeating one tool interaction, or out of steps, ends with
	// stopErr instead of running on unattended.
	var stopErr error
	limit := r.stepLimit()
	stops = append(stops, fantasy.FinishReasonIs(fantasy.FinishReasonStop), func(steps []fantasy.StepResult) bool {
		switch {
		case len(steps) >= limit:
			stopErr = errStepLimit(limit)
		case repeatsToolCalls(steps):
			stopErr = ErrToolLoop
		}
		return stopErr != nil
	})
	// what a step recorded and never ran (a finish that was no tool call, a
	// callback that failed the step) is released when the turn is over
	var recorded []string
	defer func() { tools.CallDone(tctx, recorded...) }()
	// the tool whose arguments the step was streaming: a reply cut there
	// may leave no tool call behind for closeToolCalls to answer
	pending := ""
	var usage fantasy.Usage // every finished stream's, a step that failed after it too
	var stepStart time.Time
	var firstToken time.Duration
	token := func() {
		if firstToken == 0 {
			firstToken = time.Since(stepStart)
		}
	}
	call := r.baseCall(history)
	call.StopWhen = stops
	call.OnStepStart = func(int) error {
		pending, stepStart, firstToken = "", time.Now(), 0
		hooks.StepStart()
		return nil
	}
	call.OnTextDelta = func(id, text string) error { token(); hooks.TextDelta(text); return nil }
	call.OnReasoningDelta = func(id, text string) error { token(); hooks.ReasoningDelta(text); return nil }
	call.OnToolInputStart = func(id, name string) error {
		token()
		pending = name
		hooks.ToolCallPending(id, name)
		return nil
	}
	call.OnToolInputDelta = func(id, delta string) error {
		hooks.ToolInputDelta(id, delta)
		return nil
	}
	call.OnToolCall = func(tc fantasy.ToolCallContent) error {
		if tools.CallStart(tctx, tc.ToolCallID, tc.ToolName, tc.Input) {
			recorded = append(recorded, tc.ToolCallID)
		}
		hooks.ToolCallStart(tc.ToolCallID, tc.ToolName, tc.Input)
		return nil
	}
	call.OnToolResult = func(res fantasy.ToolResultContent) error {
		// fires for a call fantasy refused to run too, which Run never releases
		tools.CallDone(tctx, res.ToolCallID)
		hooks.ToolCallEnd(res.ToolCallID, res.ToolName, res.Result, res.ClientMetadata)
		return nil
	}
	call.OnStreamFinish = func(u fantasy.Usage, _ fantasy.FinishReason, _ fantasy.ProviderMetadata) error {
		usage.InputTokens += u.InputTokens
		usage.OutputTokens += u.OutputTokens
		usage.TotalTokens += u.TotalTokens
		// cache reads and writes are billed apart from input, and providers
		// report them apart: keep them for the price
		usage.CacheReadTokens += u.CacheReadTokens
		usage.CacheCreationTokens += u.CacheCreationTokens
		hooks.StreamDone(u)
		return nil
	}
	call.OnRetry = func(_ *fantasy.ProviderError, delay time.Duration) {
		firstToken = 0 // the call streams again from its first delta
		hooks.Retrying(r.last.Err, delay)
	}
	// Each finished step and each steered message is stored as it happens, so
	// a turn that fails or is cancelled later keeps the work done before.
	var steps []fantasy.StepResult
	var steered []steeredMsg
	var persistErr error
	save := func(msgs ...fantasy.Message) {
		for _, m := range msgs {
			if persist != nil && persistErr == nil {
				persistErr = persist(m)
			}
		}
	}
	call.OnStepFinish = func(s fantasy.StepResult) error {
		steps = append(steps, s)
		save(s.Messages...)
		hooks.StepDone(Step{N: len(steps), Usage: s.Usage, Took: time.Since(stepStart), FirstToken: firstToken, Tools: len(s.Content.ToolCalls())})
		return nil
	}
	// fantasy rebuilds each step's input as initial prompt + step responses,
	// so steered messages are re-inserted every step at their response offset.
	steer, active := r.Steer, r.ActiveTools
	call.PrepareStep = func(ctx context.Context, o fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		if persistErr != nil {
			return ctx, fantasy.PrepareStepResult{}, persistErr
		}
		resp := stepMessages(o.Steps)
		if steer != nil {
			for _, text := range steer() {
				m := r.compose(text)
				steered = append(steered, steeredMsg{at: len(resp), msg: m})
				save(m)
			}
		}
		msgs := o.Messages
		if len(steered) > 0 {
			msgs = interleave(o.Messages[:len(o.Messages)-len(resp)], resp, steered)
		}
		var res fantasy.PrepareStepResult
		if active != nil {
			res.ActiveTools = active()
		}
		// a fork's first request offers the caller's own list, once
		if r.carried != nil {
			res.ActiveTools, r.carried = r.carried, nil
		}
		res.Messages = withCacheBreakpoints(r.signed(msgs, res.ActiveTools, true))
		return ctx, res, nil
	}

	_, err := r.agent.Stream(tctx, call)
	if err == nil {
		err = persistErr
	}
	if err == nil && len(steps) > 0 {
		// fantasy runs a step's calls only on a tool-calls finish and records
		// the rest unrun; on plain text a gateway may send no reason or "other"
		last, note := steps[len(steps)-1], "The reply hit the output limit while writing the "+pending+" call, so it never ran. Resend the work in smaller calls: for a large file, write the first part, then add the rest with edit."
		calls := pending != "" || len(last.Content.ToolCalls()) > 0
		switch reason := last.FinishReason; {
		case reason == fantasy.FinishReasonLength:
			err = ErrReplyCut
		case reason == fantasy.FinishReasonError || reason == fantasy.FinishReasonContentFilter ||
			reason != fantasy.FinishReasonToolCalls && calls:
			err = fmt.Errorf("the provider ended the reply early (%s)", reason)
			note = "The provider ended the reply (" + string(reason) + ") while writing the " + pending + " call, so it never ran."
		}
		// some providers drop a call cut mid-arguments from the step, so the
		// history would show nothing and the model repeats the same call
		if err != nil && pending != "" && len(last.Content.ToolCalls()) == 0 {
			m := r.compose(Note(note))
			steered = append(steered, steeredMsg{at: len(stepMessages(steps)), msg: m})
			save(m)
		}
	}
	if err == nil {
		err = stopErr
	}
	// Completed steps (assistant content, tool results, steered user
	// messages) join history in the order the model saw them, also after an
	// error; the step that failed is dropped whole, so no call lacks its result.
	r.mu.Lock()
	r.cut = cut && err == nil
	r.History = append(r.History, interleave(nil, stepMessages(steps), steered)...)
	r.mu.Unlock()
	hooks.TurnDone(err, usage)
	return err
}

// Snapshot is the conversation as the model last saw it, the running
// turn's finished steps included.
func (r *Runner) Snapshot() []fantasy.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active && r.live != nil {
		return r.live
	}
	return slices.Clone(r.History)
}

// Tools is the tool set the last request sent, in order: a fork rebuilds it so
// the prefix the provider cached still matches.
func (r *Runner) Tools() []fantasy.ToolInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]fantasy.ToolInfo, len(r.tools))
	for i, t := range r.tools {
		out[i] = t.Info()
	}
	return out
}

// Fork builds the runner a fork of this conversation sends on: this runner's
// model, system prompt, context message and request options, tools of its own,
// and the messages and tool names this one last sent, which its first request
// repeats. Hooks, Persist, Steer, ActiveTools and Full belong to the run, so
// the caller sets them.
func (r *Runner) Fork(tools []fantasy.AgentTool) (*Runner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live == nil {
		return nil, errors.New("agent: nothing sent yet")
	}
	f := NewRunner(r.last.LanguageModel, tools, r.system)
	f.Context, f.Temperature, f.TextOnly = r.Context, r.Temperature, r.TextOnly
	f.ProviderOptions, f.Headers = r.ProviderOptions, r.Headers
	// sent is the history itself: keepSigned compares the two, and a fork sends
	// the same bytes, the signed thinking included
	f.History, f.sent, f.key = slices.Clone(r.live), slices.Clone(r.live), r.key
	// the tools that request offered, which the fork's first request repeats
	f.carried = slices.Clone(r.offered)
	return f, nil
}

// Aside answers text after history without joining it (/btw): one step on
// this runner's system prompt, tools and cache breakpoints, so the provider's
// prefix cache holds. Tool choice stays as a turn sends it, since Anthropic
// drops the message cache when it changes; the tools refuse to run instead.
// It streams: Anthropic's SDK refuses a large max_tokens otherwise, and the
// idle timer covers only a stream.
func (r *Runner) Aside(ctx context.Context, history []fantasy.Message, text string) (*fantasy.AgentResult, error) {
	msgs := append(closeToolCalls(history), UserMessage(text))
	if r.TextOnly {
		msgs = stripImages(msgs, noImages)
	}
	inert := make([]fantasy.AgentTool, len(r.tools))
	for i, t := range r.tools {
		inert[i] = inertTool{t}
	}
	call := r.baseCall(msgs)
	call.StopWhen = []fantasy.StopCondition{fantasy.StepCountIs(1)}
	// past lastErr, which the turn this may run beside writes
	call.ModelProvider = func() fantasy.LanguageModel { return r.last.LanguageModel }
	call.PrepareStep = func(ctx context.Context, o fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		res := fantasy.PrepareStepResult{Tools: inert}
		if r.ActiveTools != nil {
			res.ActiveTools = r.ActiveTools()
		}
		res.Messages = withCacheBreakpoints(r.signed(o.Messages, res.ActiveTools, false))
		return ctx, res, nil
	}
	return r.agent.Stream(ctx, call)
}

// baseCall is what every request of the runner carries.
func (r *Runner) baseCall(msgs []fantasy.Message) fantasy.AgentStreamCall {
	if r.Context != "" {
		msgs = append([]fantasy.Message{UserMessage(r.Context)}, msgs...)
	}
	call := fantasy.AgentStreamCall{
		Messages: msgs, Temperature: r.Temperature, MaxRetries: &maxRetries,
		ProviderOptions: r.ProviderOptions, Headers: r.Headers,
	}
	if n := r.MaxOutputTokens; n > 0 {
		call.MaxOutputTokens = &n
	}
	return call
}

// inertTool sends a tool's schema and refuses to run it.
type inertTool struct{ fantasy.AgentTool }

func (inertTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextErrorResponse("a side question runs no tools"), nil
}

const (
	// maxSteps bounds one turn when agent.max_steps says nothing.
	maxSteps = 200
	// A loop is the same block of steps repeated: loopPeriod bounds how
	// long that block may be, and a run counts once it covers loopMinSteps
	// steps or loopMinCycles repeats, whichever is more.
	loopPeriod    = 3
	loopMinSteps  = 6
	loopMinCycles = 3
)

// pollingTools are meant to be called again with the same input: waiting on
// a job is not a loop, it is how the model watches one. Such a step is left
// out of the comparison entirely, so one poll cannot break a real loop's run.
var pollingTools = map[string]bool{"job_read": true, "job_list": true}

// maxRetries is the provider retries a step gets; fantasy's default of 3
// gives up on an overloaded provider inside a minute.
var maxRetries = 5

// ErrToolLoop ends a turn the model has wedged: see repeatsToolCalls.
var ErrToolLoop = errors.New("stopped: the agent kept repeating the same tool calls with the same results")

// errStepLimit is how a turn ends once it has taken limit steps: it names the
// setting, so the user knows which knob stopped it.
func errStepLimit(limit int) error {
	return fmt.Errorf("stopped at the %d-step limit (agent.max_steps): send a message to continue", limit)
}

// stepLimit is the steps one turn may take: what the host configured, else
// the default, so a long unattended run cannot go on forever.
func (r *Runner) stepLimit() int {
	if r.MaxSteps > 0 {
		return r.MaxSteps
	}
	return maxSteps
}

// repeatsToolCalls reports whether the latest tool steps repeat one block of
// calls with the same results (see repeatsCycle). Stuck models repeat
// themselves immediately; an edit-build-edit cycle whose build keeps printing
// nothing does not, which is why the repeats must be consecutive.
func repeatsToolCalls(steps []fantasy.StepResult) bool {
	var sigs []string
	for _, s := range steps {
		calls := s.Content.ToolCalls()
		if len(calls) == 0 {
			continue
		}
		sig, polling := "", false
		for _, c := range calls {
			if pollingTools[c.ToolName] {
				polling = true
				break
			}
			sig += c.ToolName + "\x00" + c.Input + "\x00"
		}
		if polling {
			continue // waiting on a job says nothing about progress
		}
		for _, res := range s.Content.ToolResults() {
			sig += ToolOutput(res.Result) + "\x00"
		}
		sigs = append(sigs, sig)
	}
	for period := 1; period <= loopPeriod; period++ {
		if repeatsCycle(sigs, period) {
			return true
		}
	}
	return false
}

// repeatsCycle reports whether the end of sigs is one block of period steps
// repeated: a model stuck on one call, or cycling a short pattern such as
// the same edit and the same failing test.
func repeatsCycle(sigs []string, period int) bool {
	n := period * max(loopMinSteps/period, loopMinCycles)
	if len(sigs) < n {
		return false
	}
	last := sigs[len(sigs)-n:]
	for i := period; i < n; i++ {
		if last[i] != last[i%period] {
			return false
		}
	}
	return true
}

// ContextUsed is the context one model call took: providers report cache
// reads and writes apart from input, and the reply joins the next prompt.
func ContextUsed(u fantasy.Usage) int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens + u.OutputTokens
}

// withCacheControl is a copy of o that also marks an Anthropic prompt cache
// breakpoint; the openrouter and vercel providers read it too, the rest
// ignore options keyed by another provider's name.
func withCacheControl(o fantasy.ProviderOptions) fantasy.ProviderOptions {
	out := fantasy.ProviderOptions{}
	maps.Copy(out, o)
	out[anthropic.Name] = &anthropic.ProviderCacheControlOptions{CacheControl: anthropic.CacheControl{Type: "ephemeral"}}
	return out
}

// signed drops the thinking whose signed prefix (system, tool set, earlier
// messages) changed: Anthropic, openrouter and vercel refuse it. Every request
// passes here, so this one check covers every history edit.
func (r *Runner) signed(msgs []fantasy.Message, active []string, record bool) []fantasy.Message {
	// the prefix: the system prompt and the Context message baseCall put after it
	n := 0
	if len(msgs) > 0 && msgs[0].Role == fantasy.MessageRoleSystem {
		n = 1
	}
	if r.Context != "" && len(msgs) > n {
		n++
	}
	sys, msgs := msgs[:n], msgs[n:]
	var key strings.Builder
	for _, m := range sys {
		for _, p := range m.Content {
			if t, ok := p.(fantasy.TextPart); ok {
				key.WriteString(t.Text)
			}
		}
	}
	names := active
	if names == nil {
		for _, t := range r.tools {
			names = append(names, t.Info().Name)
		}
	}
	// Sorted: the API binds tools as a set and deferMCP's order is random.
	// NOTE: tools by name: a schema that changes under the same name is missed
	for _, n := range slices.Sorted(slices.Values(names)) {
		key.WriteString("\x00" + n)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out, rec := keepSigned(msgs, r.sent, key.String() == r.key)
	if record {
		r.sent, r.key, r.live, r.offered = rec, key.String(), out, names
	}
	return slices.Concat(sys, out)
}

// keepSigned drops the reasoning from msgs starting at the first message that
// differs from sent, all of it when the prefix changed. A message sent without
// its reasoning matches its stripped form, so a drop holds on later steps
// while the messages after it keep their own. out keeps the open tool loop
// (past the last user text that follows no tool result) whole: Anthropic
// refuses a final tool_use without its thinking on every account, a stale
// signature only where it is enforced.
// rec, the next step's sent, strips it too, so it goes once the loop closes.
// NOTE: a prefix that changes inside a turn (tool_search loading an MCP tool,
// a compaction continuation, a subagent prune) leaves the open loop signed
// over the old one, which an enforcing account refuses; freezing ActiveTools
// for the turn or the drop_block beta would lift it.
func keepSigned(msgs, sent []fantasy.Message, samePrefix bool) (out, rec []fantasy.Message) {
	rec = slices.Clone(msgs)
	edited := !samePrefix
	open, results := 0, false
	for i, m := range rec {
		// Text behind tool results (a steered message) joins their user block
		// at the API, so the tool_use before it is still the last.
		if m.Role == fantasy.MessageRoleUser && !results && slices.ContainsFunc(m.Content, func(p fantasy.MessagePart) bool { return p.GetType() == fantasy.ContentTypeText }) {
			open = i
		}
		if m.Role != fantasy.MessageRoleUser {
			results = m.Role == fantasy.MessageRoleTool
		}
		if !edited && i < len(sent) {
			if reflect.DeepEqual(m, sent[i]) {
				continue
			}
			if s := noReasoning(m); reflect.DeepEqual(s, sent[i]) {
				rec[i] = s
				continue
			}
			edited = true
		}
		if edited {
			rec[i] = noReasoning(m)
		}
	}
	return slices.Concat(rec[:open], msgs[open:]), rec
}

// noReasoning drops the Claude thinking, which openrouter and vercel keep
// under the anthropic metadata too; no other reasoning is bound to its prefix.
func noReasoning(m fantasy.Message) fantasy.Message {
	signed := func(p fantasy.MessagePart) bool {
		r, ok := p.(fantasy.ReasoningPart)
		return ok && anthropic.GetReasoningMetadata(r.ProviderOptions) != nil
	}
	if slices.ContainsFunc(m.Content, signed) {
		m.Content = slices.DeleteFunc(slices.Clone(m.Content), signed)
	}
	return m
}

// withCacheBreakpoints returns a copy of msgs with breakpoints on the system
// prompt (it follows the tools, so both are cached) and on the last two
// messages, so each step reads the previous step's prefix from the cache.
// With runningTool's that is all 4 breakpoints Anthropic allows.
func withCacheBreakpoints(msgs []fantasy.Message) []fantasy.Message {
	out := slices.Clone(msgs)
	for i := range out {
		if (i == 0 && out[i].Role == fantasy.MessageRoleSystem) || i >= len(out)-2 {
			out[i].ProviderOptions = withCacheControl(out[i].ProviderOptions)
		}
	}
	return out
}

type steeredMsg struct {
	at  int // number of step response messages that preceded it
	msg fantasy.Message
}

func (r *Runner) compose(text string) fantasy.Message {
	if r.Compose != nil {
		return r.Compose(text)
	}
	return UserMessage(text)
}

// UserMessage is one plain-text message from the user.
func UserMessage(text string) fantasy.Message {
	return fantasy.Message{
		Role:    fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}},
	}
}

func stepMessages(steps []fantasy.StepResult) []fantasy.Message {
	var out []fantasy.Message
	for _, s := range steps {
		out = append(out, s.Messages...)
	}
	return out
}

// interleave returns head + resp with each steered message placed before
// the response message at its offset.
func interleave(head, resp []fantasy.Message, steered []steeredMsg) []fantasy.Message {
	out := slices.Clone(head)
	i := 0
	for n, m := range resp {
		for ; i < len(steered) && steered[i].at <= n; i++ {
			out = append(out, steered[i].msg)
		}
		out = append(out, m)
	}
	for ; i < len(steered); i++ {
		out = append(out, steered[i].msg)
	}
	return out
}

// ToolOutput is the text of a tool result: the output, or the error message
// of a failed call.
func ToolOutput(c fantasy.ToolResultOutputContent) string {
	if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](c); ok {
		return txt.Text
	}
	if e, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](c); ok && e.Error != nil {
		return e.Error.Error()
	}
	return ""
}

// closeToolCalls inserts an error result after every tool call that has
// none: fantasy records the calls of a reply cut off by the output limit or
// a provider error without running them, and providers refuse such history.
func closeToolCalls(history []fantasy.Message) []fantasy.Message {
	out := make([]fantasy.Message, 0, len(history))
	for i, m := range history {
		out = append(out, m)
		if m.Role != fantasy.MessageRoleAssistant {
			continue
		}
		answered := map[string]bool{}
		if i+1 < len(history) && history[i+1].Role == fantasy.MessageRoleTool {
			for _, p := range history[i+1].Content {
				if res, ok := p.(fantasy.ToolResultPart); ok {
					answered[res.ToolCallID] = true
				}
			}
		}
		var missing []fantasy.MessagePart
		for _, p := range m.Content {
			if c, ok := p.(fantasy.ToolCallPart); ok && !c.ProviderExecuted && !answered[c.ToolCallID] {
				missing = append(missing, fantasy.ToolResultPart{ToolCallID: c.ToolCallID, Output: fantasy.ToolResultOutputContentError{Error: errNotRun}})
			}
		}
		if len(missing) > 0 {
			out = append(out, fantasy.Message{Role: fantasy.MessageRoleTool, Content: missing})
		}
	}
	return out
}

var errNotRun = errors.New("not run: the reply was cut off")

// noImages is why TextOnly strips an image.
const noImages = "the current model does not take images"

// DropImages replaces every image in the history with a note naming why,
// for a request the provider refused over one image's size.
func (r *Runner) DropImages(why string) {
	r.mu.Lock()
	r.History = stripImages(r.History, why)
	r.mu.Unlock()
}

// stripImages replaces every image in history with text naming why. It
// copies a message's Content before changing it: a snapshot or a running
// step may share it.
func stripImages(history []fantasy.Message, why string) []fantasy.Message {
	for i, m := range history {
		copied := false
		for j, part := range m.Content {
			var note fantasy.MessagePart
			switch p := part.(type) {
			case fantasy.FilePart:
				note = fantasy.TextPart{Text: "[image " + p.Filename + " omitted: " + why + "]"}
			case fantasy.ToolResultPart:
				if p.Output != nil && p.Output.GetType() == fantasy.ToolResultContentTypeMedia {
					p.Output = fantasy.ToolResultOutputContentError{Error: errors.New("image omitted: " + why + "; go on without it, or ask the user to describe it")}
					note = p
				}
			}
			if note == nil {
				continue
			}
			if !copied {
				history[i].Content, copied = slices.Clone(m.Content), true
			}
			history[i].Content[j] = note
		}
	}
	return history
}

func isErrResult(c fantasy.ToolResultContent) bool {
	return ErrResult(c.Result)
}

// ErrResult is whether a tool result is the error one: a call that did not
// do what it was asked answers with it, whatever the text says.
func ErrResult(c fantasy.ToolResultOutputContent) bool {
	return c != nil && c.GetType() == fantasy.ToolResultContentTypeError
}
