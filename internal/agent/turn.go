package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"charm.land/fantasy"
	anthropic "charm.land/fantasy/providers/anthropic"
)

// StreamHooks receives streaming updates; implementations must be safe
// to call from the streaming goroutine.
type StreamHooks interface {
	TextDelta(text string)
	ReasoningDelta(text string)
	ToolCallStart(id, name, input string)
	ToolCallEnd(id, name string, ok bool, output string)
	// StepStart marks the start of a model call; after Retrying the call
	// streams again from its first delta.
	StepStart()
	// Retrying reports a provider retry (rate limit, overload) before the
	// backoff delay.
	Retrying(err error, delay time.Duration)
	TurnDone(err error, usage fantasy.Usage)
}

// PersistFunc receives every message of the turn (user first, then the
// step messages) as serializable fantasy messages; role is the message
// role.
type PersistFunc func(role string, msg fantasy.Message) error

// Runner runs one conversation: history in, streaming turns out. Send is
// safe for one concurrent turn; a second Send while one runs returns
// ErrTurnActive (mid-turn messages go through Steer instead, DESIGN §4).
type Runner struct {
	mu          sync.Mutex
	agent       fantasy.Agent
	History     []fantasy.Message
	Hooks       StreamHooks
	Persist     PersistFunc
	Temperature *float64

	// MaxOutputTokens caps each reply; 0 leaves the provider default.
	MaxOutputTokens int64

	// ProviderOptions ride on every call (reasoning effort).
	ProviderOptions fantasy.ProviderOptions

	// Steer is polled at every step boundary; the messages it returns join
	// the running turn as user messages (DESIGN §4 steering).
	Steer func() []string

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

// ErrReplyCut ends a turn whose last reply hit the output token limit.
var ErrReplyCut = errors.New("the reply hit the output token limit (the model's max_output) and was cut off")

// NewRunner builds a runner over a resolved model.
func NewRunner(model fantasy.LanguageModel, tools []fantasy.AgentTool, system string) *Runner {
	opts := []fantasy.AgentOption{}
	if system != "" {
		opts = append(opts, fantasy.WithSystemPrompt(system))
	}
	if len(tools) > 0 {
		opts = append(opts, fantasy.WithTools(tools...))
	}
	return &Runner{agent: fantasy.NewAgent(model, opts...)}
}

// Cancel aborts the active turn, if any.
func (r *Runner) Cancel() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
}

// Active reports whether a turn is running.
func (r *Runner) Active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
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
	r.active, r.cut = true, false
	tctx, cancel := context.WithCancel(ctx)
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
			if err := persist(string(msg.Role), msg); err != nil {
				hooks.TurnDone(err, fantasy.Usage{})
				return err
			}
		}
		userMsg = append(userMsg, msg)
	}
	r.mu.Lock()
	r.History = append(closeToolCalls(r.History), userMsg...)
	if r.TextOnly {
		r.History = stripImages(r.History)
	}
	history := append([]fantasy.Message(nil), r.History...)
	temp := r.Temperature
	provOpts := r.ProviderOptions
	r.mu.Unlock()

	// Full goes first: fantasy stops at the first condition that holds, and
	// Full has to see every step.
	var stops []fantasy.StopCondition
	cut := false
	if full != nil {
		stops = append(stops, func(steps []fantasy.StepResult) bool {
			last := steps[len(steps)-1]
			stop := full(ContextUsed(last.Usage)) && last.FinishReason == fantasy.FinishReasonToolCalls
			cut = cut || stop
			return stop
		})
	}
	// A turn stuck repeating one tool interaction, or out of steps, ends with
	// stopErr instead of running on unattended.
	var stopErr error
	stops = append(stops, fantasy.FinishReasonIs(fantasy.FinishReasonStop), func(steps []fantasy.StepResult) bool {
		switch {
		case len(steps) >= maxSteps:
			stopErr = ErrStepLimit
		case repeatsToolCalls(steps):
			stopErr = ErrToolLoop
		}
		return stopErr != nil
	})
	call := fantasy.AgentStreamCall{
		Messages:         history,
		StopWhen:         stops,
		OnStepStart:      func(int) error { hooks.StepStart(); return nil },
		OnTextDelta:      func(id, text string) error { hooks.TextDelta(text); return nil },
		OnReasoningDelta: func(id, text string) error { hooks.ReasoningDelta(text); return nil },
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			hooks.ToolCallStart(tc.ToolCallID, tc.ToolName, tc.Input)
			return nil
		},
		OnToolResult: func(res fantasy.ToolResultContent) error {
			hooks.ToolCallEnd(res.ToolCallID, res.ToolName, !isErrResult(res), ToolOutput(res.Result))
			return nil
		},
		OnRetry:         func(err *fantasy.ProviderError, delay time.Duration) { hooks.Retrying(retryErr(err), delay) },
		MaxRetries:      &maxRetries,
		ProviderOptions: provOpts,
	}
	if temp != nil {
		t := *temp
		call.Temperature = &t
	}
	if n := r.MaxOutputTokens; n > 0 {
		call.MaxOutputTokens = &n
	}
	// Each finished step and each steered message is stored as it happens, so
	// a turn that fails or is cancelled later keeps the work done before.
	var steps []fantasy.StepResult
	var steered []steeredMsg
	var persistErr error
	save := func(msgs ...fantasy.Message) {
		for _, m := range msgs {
			if persist != nil && persistErr == nil {
				persistErr = persist(string(m.Role), m)
			}
		}
	}
	call.OnStepFinish = func(s fantasy.StepResult) error {
		steps = append(steps, s)
		save(s.Messages...)
		return nil
	}
	// fantasy rebuilds each step's input as initial prompt + step responses,
	// so steered messages are re-inserted every step at their response offset.
	steer := r.Steer
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
		return ctx, fantasy.PrepareStepResult{Messages: withCacheBreakpoints(msgs)}, nil
	}

	_, err := r.agent.Stream(tctx, call)
	if err == nil {
		err = persistErr
	}
	if err == nil && len(steps) > 0 && steps[len(steps)-1].FinishReason == fantasy.FinishReasonLength {
		err = ErrReplyCut
	}
	if err == nil {
		err = stopErr
	}
	// Completed steps (assistant content, tool results, steered user
	// messages) join history in the order the model saw them, also after an
	// error; the step that failed is dropped whole, so no call lacks its result.
	var usage fantasy.Usage
	for _, s := range steps {
		usage.InputTokens += s.Usage.InputTokens
		usage.OutputTokens += s.Usage.OutputTokens
		usage.TotalTokens += s.Usage.TotalTokens
		// cache reads and writes are billed apart from input, and providers
		// report them apart: keep them for the price
		usage.CacheReadTokens += s.Usage.CacheReadTokens
		usage.CacheCreationTokens += s.Usage.CacheCreationTokens
	}
	r.mu.Lock()
	r.cut = cut && err == nil
	r.History = append(r.History, interleave(nil, stepMessages(steps), steered)...)
	r.mu.Unlock()
	hooks.TurnDone(err, usage)
	return err
}

const (
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

var (
	ErrStepLimit = fmt.Errorf("stopped after %d steps: send a message to continue", maxSteps)
	ErrToolLoop  = errors.New("stopped: the agent kept repeating the same tool calls with the same results")
)

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
			txt, _ := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](res.Result)
			sig += txt.Text + "\x00"
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

// retryErr converts fantasy's retry cause to an error. fantasy passes a nil
// *ProviderError for retryable errors of other types (network failures);
// wrapped as-is it would be a non-nil error whose Error() panics.
func retryErr(err *fantasy.ProviderError) error {
	if err == nil {
		return nil
	}
	return err
}

// cacheControl marks an Anthropic prompt cache breakpoint; other providers
// ignore options keyed by another provider's name.
var cacheControl = fantasy.ProviderOptions{
	anthropic.Name: &anthropic.ProviderCacheControlOptions{CacheControl: anthropic.CacheControl{Type: "ephemeral"}},
}

// withCacheBreakpoints returns a copy of msgs with breakpoints on the system
// prompt (it follows the tools, so both are cached) and on the last two
// messages, so each step reads the previous step's prefix from the cache.
// That is 3 of the 4 breakpoints Anthropic allows.
func withCacheBreakpoints(msgs []fantasy.Message) []fantasy.Message {
	out := slices.Clone(msgs)
	for i := range out {
		if (i == 0 && out[i].Role == fantasy.MessageRoleSystem) || i >= len(out)-2 {
			out[i].ProviderOptions = cacheControl
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
	out := append([]fantasy.Message(nil), head...)
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

// stripImages replaces every image in history with text saying so, for a
// model that refuses image parts.
func stripImages(history []fantasy.Message) []fantasy.Message {
	for i, m := range history {
		for j, part := range m.Content {
			switch p := part.(type) {
			case fantasy.FilePart:
				history[i].Content[j] = fantasy.TextPart{Text: "[image " + p.Filename + " omitted: the current model does not take images]"}
			case fantasy.ToolResultPart:
				if p.Output != nil && p.Output.GetType() == fantasy.ToolResultContentTypeMedia {
					p.Output = fantasy.ToolResultOutputContentError{Error: errNoImages}
					history[i].Content[j] = p
				}
			}
		}
	}
	return history
}

var errNoImages = errors.New("image omitted: the current model does not take images; go on without it, or ask the user to describe it or switch model")

func isErrResult(c fantasy.ToolResultContent) bool {
	if c.Result == nil {
		return false
	}
	return c.Result.GetType() == fantasy.ToolResultContentTypeError
}
