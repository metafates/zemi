package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"charm.land/fantasy"
)

// StreamHooks receives streaming updates; implementations must be safe
// to call from the streaming goroutine.
type StreamHooks interface {
	TextDelta(text string)
	ReasoningDelta(text string)
	ToolCallStart(id, name, input string)
	ToolCallEnd(id, name string, ok bool, output string)
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
// ErrTurnActive (the UI steers via TurnInput instead, DESIGN §8).
type Runner struct {
	mu          sync.Mutex
	agent       fantasy.Agent
	History     []fantasy.Message
	Hooks       StreamHooks
	Persist     PersistFunc
	Temperature *float64

	// ProviderOptions ride on every call (reasoning effort).
	ProviderOptions fantasy.ProviderOptions

	// Steer is polled at every step boundary; the messages it returns join
	// the running turn as user messages (DESIGN §4 steering).
	Steer func() []string

	// Compose builds the user message for a sent or steered text (the host
	// expands attachments and images); nil sends the text as is.
	Compose func(text string) fantasy.Message

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
var ErrTurnActive = fmt.Errorf("agent: a turn is already active")

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
	r.History = append(r.History, userMsg...)
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
	call := fantasy.AgentStreamCall{
		Messages:         history,
		StopWhen:         append(stops, fantasy.FinishReasonIs(fantasy.FinishReasonStop)),
		OnTextDelta:      func(id, text string) error { hooks.TextDelta(text); return nil },
		OnReasoningDelta: func(id, text string) error { hooks.ReasoningDelta(text); return nil },
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			hooks.ToolCallStart(tc.ToolCallID, tc.ToolName, tc.Input)
			return nil
		},
		OnToolResult: func(res fantasy.ToolResultContent) error {
			outText := ""
			if txt, isText := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](res.Result); isText {
				outText = txt.Text
			}
			hooks.ToolCallEnd(res.ToolCallID, res.ToolName, !isErrResult(res), outText)
			return nil
		},
		OnRetry:         func(err *fantasy.ProviderError, delay time.Duration) { hooks.Retrying(retryErr(err), delay) },
		ProviderOptions: provOpts,
	}
	if temp != nil {
		t := *temp
		call.Temperature = &t
	}
	// fantasy rebuilds each step's input as initial prompt + step responses,
	// so steered messages are re-inserted every step at their response offset.
	var steered []steeredMsg
	steer := r.Steer
	call.PrepareStep = func(ctx context.Context, o fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		resp := stepMessages(o.Steps)
		if steer != nil {
			for _, text := range steer() {
				steered = append(steered, steeredMsg{at: len(resp), msg: r.compose(text)})
			}
		}
		if len(steered) == 0 {
			return ctx, fantasy.PrepareStepResult{}, nil
		}
		head := o.Messages[:len(o.Messages)-len(resp)]
		return ctx, fantasy.PrepareStepResult{Messages: interleave(head, resp, steered)}, nil
	}

	result, err := r.agent.Stream(tctx, call)
	if err != nil {
		hooks.TurnDone(err, fantasy.Usage{})
		return err
	}
	// step messages (assistant content, tool results, steered user
	// messages) join history in the order the model saw them
	turnMsgs := interleave(nil, stepMessages(result.Steps), steered)
	var usage fantasy.Usage
	r.mu.Lock()
	r.cut = cut
	r.History = append(r.History, turnMsgs...)
	for _, step := range result.Steps {
		usage.InputTokens += step.Usage.InputTokens
		usage.OutputTokens += step.Usage.OutputTokens
		usage.TotalTokens += step.Usage.TotalTokens
	}
	r.mu.Unlock()
	if persist != nil {
		for _, m := range turnMsgs {
			if err := persist(string(m.Role), m); err != nil {
				hooks.TurnDone(err, usage)
				return err
			}
		}
	}
	hooks.TurnDone(nil, usage)
	return nil
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

type steeredMsg struct {
	at  int // number of step response messages that preceded it
	msg fantasy.Message
}

func (r *Runner) compose(text string) fantasy.Message {
	if r.Compose != nil {
		return r.Compose(text)
	}
	return userMessage(text)
}

func userMessage(text string) fantasy.Message {
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

func isErrResult(c fantasy.ToolResultContent) bool {
	if c.Result == nil {
		return false
	}
	return c.Result.GetType() == fantasy.ToolResultContentTypeError
}
