package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	openai "charm.land/fantasy/providers/openai"
)

// mockModel replays scripted stream parts; steps, when set, scripts one
// parts list per model call.
type mockModel struct {
	mu       sync.Mutex
	parts    []fantasy.StreamPart
	steps    [][]fantasy.StreamPart
	lastCall fantasy.Call
	block    func(ctx context.Context) // optional gate inside Stream
}

func (m *mockModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *mockModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	m.lastCall = call
	parts := m.parts
	if len(m.steps) > 0 {
		parts, m.steps = m.steps[0], m.steps[1:]
	}
	block := m.block
	m.mu.Unlock()
	if block != nil {
		block(ctx)
	}
	return func(yield func(fantasy.StreamPart) bool) {
		for _, p := range parts {
			if !yield(p) {
				return
			}
		}
	}, nil
}

func (m *mockModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *mockModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *mockModel) Provider() string { return "mock" }
func (m *mockModel) Model() string    { return "mock-1" }

func textParts(texts ...string) []fantasy.StreamPart {
	var out []fantasy.StreamPart
	out = append(out, fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "t1"})
	for _, t := range texts {
		out = append(out, fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: t})
	}
	out = append(out, fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"})
	out = append(out, fantasy.StreamPart{
		Type: fantasy.StreamPartTypeFinish,
		Usage: fantasy.Usage{
			InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
		},
		FinishReason: fantasy.FinishReasonStop,
	})
	return out
}

type recHooks struct {
	mu       sync.Mutex
	text     strings.Builder
	tools    []string
	steps    int
	doneErr  error
	usage    fantasy.Usage
	doneWait chan struct{}
}

func newRecHooks() *recHooks {
	return &recHooks{doneWait: make(chan struct{})}
}

func (h *recHooks) TextDelta(s string) {
	h.mu.Lock()
	h.text.WriteString(s)
	h.mu.Unlock()
}

func (h *recHooks) ReasoningDelta(string) {}

func (h *recHooks) Retrying(error, time.Duration) {}

func (h *recHooks) StepStart() {}

func (h *recHooks) ToolInputDelta(string, string) {}

func (h *recHooks) ToolCallPending(id, name string) {
	h.mu.Lock()
	h.tools = append(h.tools, "pending:"+name)
	h.mu.Unlock()
}

func (h *recHooks) ToolCallStart(id, name, input string) {
	h.mu.Lock()
	h.tools = append(h.tools, "start:"+name)
	h.mu.Unlock()
}

func (h *recHooks) ToolCallRunning(string) {
	h.mu.Lock()
	h.tools = append(h.tools, "running")
	h.mu.Unlock()
}

func (h *recHooks) ToolCallEnd(id, name string, ok bool, output, _ string) {
	h.mu.Lock()
	h.tools = append(h.tools, "end:"+name)
	h.mu.Unlock()
}

func (h *recHooks) StepDone(st Step) {
	usage := st.Usage
	h.mu.Lock()
	h.steps++
	h.usage.InputTokens += usage.InputTokens
	h.usage.OutputTokens += usage.OutputTokens
	h.usage.TotalTokens += usage.TotalTokens
	h.mu.Unlock()
}

func (h *recHooks) TurnDone(err error, usage fantasy.Usage) {
	h.mu.Lock()
	h.doneErr = err
	h.usage = usage
	h.mu.Unlock()
	close(h.doneWait)
}

func waitDone(t *testing.T, h *recHooks) {
	t.Helper()
	<-h.doneWait
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.doneErr != nil {
		t.Fatalf("turn error: %v", h.doneErr)
	}
}

func TestTurnStreamsAndPersists(t *testing.T) {
	m := &mockModel{parts: textParts("hel", "lo "+"world")}
	hooks := newRecHooks()
	var persisted []string
	r := NewRunner(m, nil, "sys")
	r.Hooks = hooks
	r.Persist = func(role string, msg fantasy.Message) error {
		persisted = append(persisted, role)
		return nil
	}
	if err := r.Send(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	waitDone(t, hooks)
	hooks.mu.Lock()
	out := hooks.text.String()
	usage := hooks.usage
	hooks.mu.Unlock()
	if out != "hello world" {
		t.Fatalf("text deltas: %q", out)
	}
	if usage.TotalTokens != 15 {
		t.Fatalf("usage: %+v", usage)
	}
	if len(persisted) != 2 || persisted[0] != "user" || persisted[1] != "assistant" {
		t.Fatalf("persisted: %v", persisted)
	}
	if len(r.History) != 2 {
		t.Fatalf("history: %d", len(r.History))
	}
	// fantasy folds the system prompt into the prompt head; the user
	// message must be the last message of the call.
	last := m.lastCall.Prompt[len(m.lastCall.Prompt)-1]
	if last.Role != fantasy.MessageRoleUser {
		t.Fatalf("user message must be last, got %s", last.Role)
	}
	if sys := m.lastCall.Prompt[0]; sys.Role != fantasy.MessageRoleSystem || sys.ProviderOptions == nil || last.ProviderOptions == nil {
		t.Fatalf("system prompt and last message must be cache breakpoints: %+v", m.lastCall.Prompt)
	}
}

// TestSteerJoinsAtStepBoundary: a message steered in after a tool step
// reaches the next model call and lands in history and the store between
// the tool result and the final reply.
func TestSteerJoinsAtStepBoundary(t *testing.T) {
	m := &mockModel{steps: [][]fantasy.StreamPart{
		{
			{Type: fantasy.StreamPartTypeToolCall, ID: "c1", ToolCallName: "echo", ToolCallInput: "{}"},
			{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls},
		},
		textParts("done"),
	}}
	echo := fantasy.NewAgentTool("echo", "echo", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	r := NewRunner(m, []fantasy.AgentTool{echo}, "")
	r.Hooks = newRecHooks()
	var roles []string
	r.Persist = func(role string, _ fantasy.Message) error {
		roles = append(roles, role)
		return nil
	}
	polls := 0
	r.Steer = func() []string {
		if polls++; polls == 2 {
			return []string{"steer me"}
		}
		return nil
	}
	if err := r.Send(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(roles, ","); got != "user,assistant,tool,user,assistant" {
		t.Fatalf("persisted roles: %s", got)
	}
	last := m.lastCall.Prompt[len(m.lastCall.Prompt)-1]
	if txt, ok := last.Content[0].(fantasy.TextPart); last.Role != fantasy.MessageRoleUser || !ok || txt.Text != "steer me" {
		t.Fatalf("final call must end with the steered message, got %+v", last)
	}
	if len(r.History) != 5 {
		t.Fatalf("history: %d", len(r.History))
	}
}

// TestToolRowOpensAtInputStart: the row exists while the model is still
// streaming the call's arguments (a write's file body takes seconds), not
// only once they are complete.
func TestToolRowOpensAtInputStart(t *testing.T) {
	m := &mockModel{steps: [][]fantasy.StreamPart{
		{
			{Type: fantasy.StreamPartTypeToolInputStart, ID: "c1", ToolCallName: "echo"},
			{Type: fantasy.StreamPartTypeToolInputDelta, ID: "c1", Delta: "{}"},
			{Type: fantasy.StreamPartTypeToolInputEnd, ID: "c1"},
			{Type: fantasy.StreamPartTypeToolCall, ID: "c1", ToolCallName: "echo", ToolCallInput: "{}"},
			{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls},
		},
		textParts("done"),
	}}
	echo := fantasy.NewAgentTool("echo", "echo", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	r := NewRunner(m, []fantasy.AgentTool{echo}, "")
	hooks := newRecHooks()
	r.Hooks = hooks
	if err := r.Send(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	waitDone(t, hooks)
	if got := strings.Join(hooks.tools, ","); got != "pending:echo,start:echo,running,end:echo" {
		t.Fatalf("hook order: %s", got)
	}
}

// TestFailedTurnKeepsCompletedSteps: a step that fails after a tool step and
// a steered message keeps both in history and the store, drops only the
// failed step, and reports the finished steps' usage.
func TestFailedTurnKeepsCompletedSteps(t *testing.T) {
	m := &mockModel{steps: [][]fantasy.StreamPart{
		{
			{Type: fantasy.StreamPartTypeToolCall, ID: "c1", ToolCallName: "echo", ToolCallInput: "{}"},
			{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls, Usage: fantasy.Usage{InputTokens: 7, OutputTokens: 3}},
		},
		{{Type: fantasy.StreamPartTypeError, Error: errors.New("boom")}},
	}}
	echo := fantasy.NewAgentTool("echo", "echo", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	r := NewRunner(m, []fantasy.AgentTool{echo}, "")
	hooks := newRecHooks()
	r.Hooks = hooks
	var roles []string
	r.Persist = func(role string, _ fantasy.Message) error {
		roles = append(roles, role)
		return nil
	}
	polls := 0
	r.Steer = func() []string {
		if polls++; polls == 2 {
			return []string{"steer me"}
		}
		return nil
	}
	if err := r.Send(context.Background(), "hi"); err == nil {
		t.Fatal("want the step error")
	}
	var hist []string
	for _, msg := range r.History {
		hist = append(hist, string(msg.Role))
	}
	if got, want := strings.Join(roles, ","), "user,assistant,tool,user"; got != want || strings.Join(hist, ",") != want {
		t.Fatalf("persisted %s, history %v; want %s", got, hist, want)
	}
	if hooks.usage.InputTokens != 7 || hooks.usage.OutputTokens != 3 {
		t.Fatalf("usage: %+v", hooks.usage)
	}
}

// TestCutOffToolCallGetsResult: a reply cut off with a tool call in it
// leaves the call unrun; the next send answers it with an error result so
// the provider accepts the history.
func TestCutOffToolCallGetsResult(t *testing.T) {
	m := &mockModel{steps: [][]fantasy.StreamPart{
		{
			{Type: fantasy.StreamPartTypeToolCall, ID: "c1", ToolCallName: "echo", ToolCallInput: "{"},
			{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonLength},
		},
		textParts("done"),
	}}
	r := NewRunner(m, nil, "")
	r.MaxOutputTokens = 32000
	r.Hooks = newRecHooks()
	if err := r.Send(context.Background(), "hi"); !errors.Is(err, ErrReplyCut) {
		t.Fatalf("cut-off reply: %v, want ErrReplyCut", err)
	}
	if n := m.lastCall.MaxOutputTokens; n == nil || *n != 32000 {
		t.Fatalf("max output tokens: %v", n)
	}
	r.Hooks = newRecHooks()
	if err := r.Send(context.Background(), "go on"); err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, msg := range m.lastCall.Prompt {
		roles = append(roles, string(msg.Role))
	}
	if got := strings.Join(roles, ","); got != "user,assistant,tool,user" {
		t.Fatalf("prompt roles: %s", got)
	}
	res, ok := m.lastCall.Prompt[2].Content[0].(fantasy.ToolResultPart)
	if !ok || res.ToolCallID != "c1" || res.Output.GetType() != fantasy.ToolResultContentTypeError {
		t.Fatalf("tool message: %+v", m.lastCall.Prompt[2])
	}

	// an abnormal finish fails the turn, and a call dropped mid-arguments,
	// which leaves only its input start, gets a note naming the tool; unknown
	// on plain text is a gateway that sent no reason
	for _, c := range []struct {
		finish fantasy.FinishReason
		call   bool
		err    string
	}{
		{fantasy.FinishReasonLength, true, "output token limit"},
		{fantasy.FinishReasonError, true, "ended the reply early (error)"},
		{fantasy.FinishReasonContentFilter, false, "ended the reply early (content-filter)"},
		{fantasy.FinishReasonOther, true, "ended the reply early (other)"},
		{fantasy.FinishReasonUnknown, false, ""},
	} {
		step := textParts("partial")
		step = step[:len(step)-1]
		if c.call {
			step = append(step, fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: "c1", ToolCallName: "echo"},
				fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: "c1", Delta: "{"})
		}
		m = &mockModel{steps: [][]fantasy.StreamPart{append(step, fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: c.finish}), textParts("done")}}
		r = NewRunner(m, nil, "")
		r.Hooks = newRecHooks()
		if err := r.Send(context.Background(), "hi"); (err != nil) != (c.err != "") || err != nil && !strings.Contains(err.Error(), c.err) {
			t.Errorf("finish %s, call %v: err %v, want %q", c.finish, c.call, err, c.err)
			continue
		}
		r.Hooks = newRecHooks()
		if err := r.Send(context.Background(), "go on"); err != nil {
			t.Fatal(err)
		}
		var texts []string
		for _, msg := range m.lastCall.Prompt {
			for _, p := range msg.Content {
				if tp, ok := p.(fantasy.TextPart); ok && msg.Role == fantasy.MessageRoleUser {
					texts = append(texts, tp.Text)
				}
			}
		}
		if noted := slices.ContainsFunc(texts, func(s string) bool {
			return strings.HasPrefix(s, AttachMarker) && strings.Contains(s, "while writing the echo call")
		}); noted != c.call {
			t.Errorf("finish %s: note about the cut call %v, want %v: %q", c.finish, noted, c.call, texts)
		}
	}
}

// TestToolLoopEndsTurn: the same tool call with the same result, step after
// step, ends the turn on its sixth repeat with ErrToolLoop; a failing poll
// whose error text moves is progress.
func TestToolLoopEndsTurn(t *testing.T) {
	for _, c := range []struct {
		name string
		res  func(n int) fantasy.ToolResponse
		loop bool
	}{
		{"same result", func(int) fantasy.ToolResponse { return fantasy.NewTextResponse("same") }, true},
		{"error text moves", func(n int) fantasy.ToolResponse {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("jobs done: %d/9", n))
		}, false},
	} {
		// the text reply is the fallback: without it a missed loop would hang
		m := &mockModel{parts: textParts("done")}
		for range loopMinSteps {
			m.steps = append(m.steps, []fantasy.StreamPart{
				{Type: fantasy.StreamPartTypeToolCall, ID: "c1", ToolCallName: "echo", ToolCallInput: "{}"},
				{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls},
			})
		}
		n := 0
		echo := fantasy.NewAgentTool("echo", "echo", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			n++
			return c.res(n), nil
		})
		r := NewRunner(m, []fantasy.AgentTool{echo}, "")
		r.Hooks = newRecHooks()
		if err := r.Send(context.Background(), "hi"); errors.Is(err, ErrToolLoop) != c.loop {
			t.Errorf("%s: err = %v, want a loop %v", c.name, err, c.loop)
		}
		if n := len(r.History); c.loop && n != 1+2*loopMinSteps {
			t.Errorf("%s: history has %d messages, want the user message and %d steps", c.name, n, loopMinSteps)
		}
	}
}

// TestToolLoopCatchesTwoStepCycle: a model alternating two calls that never
// change anything (the same edit, the same failing check) is as stuck as one
// repeating a single call.
func TestToolLoopCatchesTwoStepCycle(t *testing.T) {
	step := func(input string) []fantasy.StreamPart {
		return []fantasy.StreamPart{
			{Type: fantasy.StreamPartTypeToolCall, ID: "c1", ToolCallName: "echo", ToolCallInput: input},
			{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls},
		}
	}
	// the text reply is the fallback: without it a missed loop would hang
	m := &mockModel{parts: textParts("done")}
	for range 4 {
		m.steps = append(m.steps, step(`{"a":1}`), step(`{"b":2}`))
	}
	echo := fantasy.NewAgentTool("echo", "echo", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("same"), nil
	})
	r := NewRunner(m, []fantasy.AgentTool{echo}, "")
	r.Hooks = newRecHooks()
	if err := r.Send(context.Background(), "hi"); !errors.Is(err, ErrToolLoop) {
		t.Fatalf("err = %v, want ErrToolLoop", err)
	}
}

// TestFullCutsAtStepBoundary: Full sees each step's own context (cache reads
// included, not a sum over steps), a full context ends the turn after the
// tool result, an empty Send continues without a second user message, and a
// tool that ends the turn itself is not cut.
func TestFullCutsAtStepBoundary(t *testing.T) {
	m := &mockModel{steps: [][]fantasy.StreamPart{
		{
			{Type: fantasy.StreamPartTypeToolCall, ID: "c1", ToolCallName: "echo", ToolCallInput: "{}"},
			{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls,
				Usage: fantasy.Usage{InputTokens: 10, CacheReadTokens: 90, OutputTokens: 5}},
		},
		textParts("done"),
	}}
	m.steps = append(m.steps, m.steps[0])
	stop := false
	echo := fantasy.NewAgentTool("echo", "echo", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		res := fantasy.NewTextResponse("ok")
		res.StopTurn = stop
		return res, nil
	})
	r := NewRunner(m, []fantasy.AgentTool{echo}, "")
	var used []int64
	r.Full = func(n int64) bool { used = append(used, n); return n >= 100 }
	var roles []string
	r.Persist = func(role string, _ fantasy.Message) error {
		roles = append(roles, role)
		return nil
	}
	for _, c := range []struct {
		text, roles string
		cut, stop   bool
	}{
		{"hi", "user,assistant,tool", true, false},
		{"", "user,assistant,tool,assistant", false, false},
		{"plan", "user,assistant,tool,assistant,user,assistant,tool", false, true},
	} {
		stop = c.stop
		r.Hooks = newRecHooks()
		if err := r.Send(context.Background(), c.text); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(roles, ","); got != c.roles || r.CutShort() != c.cut {
			t.Fatalf("send %q persisted roles: %s, cut %v; want %s, cut %v", c.text, got, r.CutShort(), c.roles, c.cut)
		}
	}
	if got := fmt.Sprint(used); got != "[105 15 105]" {
		t.Fatalf("Full saw %s, want [105 15 105]", got)
	}
}

// TestAsideStreams: /btw streams, since Anthropic's SDK refuses a large
// max_tokens without streaming (the mock's Generate always fails).
func TestAsideStreams(t *testing.T) {
	r := NewRunner(&mockModel{parts: textParts("answer")}, nil, "sys")
	r.MaxOutputTokens = 128_000
	res, err := r.Aside(context.Background(), nil, "what did I say")
	if err != nil || res.Response.Content.Text() != "answer" {
		t.Fatalf("aside = %+v, %v", res, err)
	}
}

func TestSecondSendWhileActive(t *testing.T) {
	block := make(chan struct{})
	m := &mockModel{
		parts: []fantasy.StreamPart{
			{Type: fantasy.StreamPartTypeTextStart, ID: "t1"},
			{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "x"},
			{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"},
			{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
		},
	}
	m.block = func(ctx context.Context) { <-block }
	hooks := newRecHooks()
	r := NewRunner(m, nil, "")
	r.Hooks = hooks
	go func() {
		_ = r.Send(context.Background(), "first")
	}()
	for !r.Active() {
	}
	if err := r.Send(context.Background(), "second"); !errors.Is(err, ErrTurnActive) {
		t.Fatalf("want ErrTurnActive, got %v", err)
	}
	close(block)
	waitDone(t, hooks)
}

func TestCancelAbortsTurn(t *testing.T) {
	m := &mockModel{
		parts: []fantasy.StreamPart{
			{Type: fantasy.StreamPartTypeTextStart, ID: "t1"},
			{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "never"},
			{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"},
			{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
		},
	}
	m.block = func(ctx context.Context) { <-ctx.Done() }
	hooks := newRecHooks()
	r := NewRunner(m, nil, "")
	r.Hooks = hooks
	go func() {
		_ = r.Send(context.Background(), "hi")
	}()
	for !r.Active() {
	}
	r.Cancel()
	// the model call blocks until its context ends, so the turn only
	// finishes if Cancel reached it
	select {
	case <-hooks.doneWait:
	case <-time.After(3 * time.Second):
		t.Fatal("Cancel did not reach the model call")
	}
}

func TestRegistryResolve(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Resolve("nosuch/model"); err == nil {
		t.Fatal("unknown provider must fail")
	}
	if _, err := r.Resolve("justone"); err == nil {
		t.Fatal("model id needs provider/model shape")
	}
	// The registry starts empty; providers come only from user config.
	if specs := r.List(); len(specs) != 0 {
		t.Fatalf("registry must start empty, got %v", specs)
	}
	err := r.Register(Spec{
		ID: "myllm", Kind: "openai_compat", BaseURL: "https://llm.internal/v1",
		Models: []Model{{ID: "main", Display: "Main"}},
	}, "k")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := r.Resolve("myllm/main"); err != nil {
		t.Fatalf("openai_compat resolve: %v", err)
	}
	// every reload replays init.lua, so registering an id again replaces it,
	// key included
	if err := r.Register(Spec{ID: "myllm", Kind: "openai"}, ""); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if specs := r.List(); len(specs) != 1 || specs[0].Kind != "openai" || r.HasKey("myllm") {
		t.Fatalf("catalog: %v, key kept: %v", specs, r.HasKey("myllm"))
	}
	// a provider with no key still resolves (the error surfaces at send)
	if _, err := r.Resolve("myllm/main"); err != nil {
		t.Fatalf("keyless resolve: %v", err)
	}
}

// TestRegistryConcurrentReload: a config reload registers while turns read
// (go test -race catches an unguarded registry).
func TestRegistryConcurrentReload(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 100 {
			_ = r.Register(Spec{ID: fmt.Sprint("p", i%3), Kind: KindOpenAI}, "k")
		}
	})
	for range 100 {
		r.HasKey("p1")
		r.CallOptions("p1/m", "high", "")
	}
	wg.Wait()
}

func TestStripImages(t *testing.T) {
	media := fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentMedia{Data: "AAAA", MediaType: "image/png"}}
	text := fantasy.ToolResultPart{ToolCallID: "c2", Output: fantasy.ToolResultOutputContentText{Text: "kept"}}
	in := []fantasy.Message{
		{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "hi"}, fantasy.FilePart{Filename: "shot.png", MediaType: "image/png", Data: []byte("x")}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{media, text}},
	}
	history := stripImages(slices.Clone(in), noImages)
	if _, ok := in[0].Content[1].(fantasy.FilePart); !ok {
		t.Fatalf("stripped the caller's shared content: %#v", in[0].Content[1])
	}
	if tp, ok := history[0].Content[1].(fantasy.TextPart); !ok || !strings.Contains(tp.Text, "shot.png omitted") {
		t.Fatalf("file part = %#v", history[0].Content[1])
	}
	if res := history[1].Content[0].(fantasy.ToolResultPart); res.Output.GetType() != fantasy.ToolResultContentTypeError {
		t.Fatalf("media result = %#v", res.Output)
	}
	if res := history[1].Content[1].(fantasy.ToolResultPart); res.Output.GetType() != fantasy.ToolResultContentTypeText {
		t.Fatalf("text result changed: %#v", res.Output)
	}
}

// TestProviderHeadersRideRequests: a gateway that needs an attribution,
// team or proxy header gets it on every request.
func TestProviderHeadersRideRequests(t *testing.T) {
	seen := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("X-Team")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"no model here"}}`)
	}))
	defer srv.Close()
	r := NewRegistry()
	err := r.Register(Spec{
		ID: "gw", Kind: KindOpenAICompat, BaseURL: srv.URL,
		Headers: map[string]string{"X-Team": "core"},
		Models:  []Model{{ID: "m"}},
	}, "k")
	if err != nil {
		t.Fatal(err)
	}
	m, err := r.Resolve("gw/m")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := m.Stream(t.Context(), fantasy.Call{Prompt: []fantasy.Message{UserMessage("hi")}})
	if err == nil {
		for range stream {
		}
	}
	select {
	case h := <-seen:
		if h != "core" {
			t.Fatalf("X-Team = %q", h)
		}
	default:
		t.Fatal("the provider never reached the gateway")
	}
}

// promptRec keeps the prompt of the last model call.
type promptRec struct {
	fantasy.LanguageModel
	prompt fantasy.Prompt
}

func (p *promptRec) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	p.prompt = call.Prompt
	return p.LanguageModel.Stream(ctx, call)
}

// TestOpenAIResponses: an openai model fantasy knows as a Responses model
// goes to /responses, a reasoning one asking for a summary and none for
// encrypted reasoning, which a model that does not reason is refused; the
// stored reasoning carries its item through JSON into the next session's
// call. Any other model goes to /chat/completions.
func TestOpenAIResponses(t *testing.T) {
	sse := func(w http.ResponseWriter, events ...string) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			fmt.Fprintf(w, "data: %s\n\n", e)
		}
	}
	var mu sync.Mutex
	var paths, bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		paths, bodies = append(paths, r.URL.Path), append(bodies, string(body))
		mu.Unlock()
		if r.URL.Path == "/responses" {
			sse(w, `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
				`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","status":"in_progress","content":[]}}`,
				`{"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"ok"}`,
				`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}}`,
				`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
			return
		}
		sse(w, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "[DONE]")
	}))
	defer srv.Close()
	reg := NewRegistry()
	if err := reg.Register(Spec{ID: "o", Kind: KindOpenAI, BaseURL: srv.URL}, "k"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		model, path, item string
		summary           bool
	}{
		{"o/gpt-5-codex", "/responses", "rs_1", true},
		{"o/gpt-4o", "/responses", "rs_1", false},
		{"o/gpt-3.5-turbo-16k", "/chat/completions", "", false},
	} {
		m, err := reg.Resolve(c.model)
		if err != nil {
			t.Fatal(err)
		}
		var stored []string
		rec := &promptRec{LanguageModel: m}
		var history []fantasy.Message
		for _, text := range []string{"hi", "again"} {
			r := NewRunner(rec, nil, "sys")
			r.History, r.Hooks, r.ProviderOptions = history, newRecHooks(), reg.CallOptions(c.model, "", "")
			r.Persist = func(_ string, msg fantasy.Message) error {
				b, err := json.Marshal(msg)
				stored = append(stored, string(b))
				return err
			}
			if err := r.Send(t.Context(), text); err != nil {
				t.Fatal(err)
			}
			waitDone(t, r.Hooks.(*recHooks))
			history = nil // a resumed session reads the store
			for _, s := range stored {
				var msg fantasy.Message
				if err := json.Unmarshal([]byte(s), &msg); err != nil {
					t.Fatal(err)
				}
				history = append(history, msg)
			}
		}
		mu.Lock()
		got, first := paths, bodies[0]
		paths, bodies = nil, nil
		mu.Unlock()
		if len(got) != 2 || got[0] != c.path || got[1] != c.path {
			t.Fatalf("%s: paths %v, want %s", c.model, got, c.path)
		}
		if strings.Contains(first, "encrypted_content") || strings.Contains(first, `"summary":"auto"`) != c.summary {
			t.Fatalf("%s: want summary %v and no encrypted reasoning: %s", c.model, c.summary, first)
		}
		item := ""
		for _, msg := range rec.prompt {
			for _, part := range msg.Content {
				if rp, ok := part.(fantasy.ReasoningPart); ok {
					if md := openai.GetReasoningMetadata(rp.ProviderOptions); md != nil {
						item = md.ItemID
					}
				}
			}
		}
		if item != c.item {
			t.Fatalf("%s: turn 2 carried reasoning item %q, want %q", c.model, item, c.item)
		}
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestCacheBreakpoints: an anthropic request marks the system block, the
// last two messages and the last tool; an openrouter one goes to its fixed
// URL with the same messages marked (fantasy drops a tool's mark there).
func TestCacheBreakpoints(t *testing.T) {
	var mu sync.Mutex
	var sent, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"no model here"}}`)
	}))
	defer srv.Close()
	orig := modelHTTP
	t.Cleanup(func() { modelHTTP = orig })
	modelHTTP = func() *http.Client {
		return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			sent = r.URL.String()
			mu.Unlock()
			r = r.Clone(r.Context())
			r.URL.Scheme, r.URL.Host = "http", strings.TrimPrefix(srv.URL, "http://")
			return http.DefaultTransport.RoundTrip(r)
		})}
	}
	reg := NewRegistry()
	for _, s := range []Spec{{ID: "a", Kind: KindAnthropic}, {ID: "or", Kind: KindOpenRouter}} {
		if err := reg.Register(s, "k"); err != nil {
			t.Fatal(err)
		}
	}
	noop := func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.ToolResponse{}, nil
	}
	tools := []fantasy.AgentTool{fantasy.NewAgentTool("one", "one", noop), fantasy.NewAgentTool("two", "two", noop)}
	marks := func(list []json.RawMessage) string {
		out := ""
		for _, v := range list {
			out += map[bool]string{true: "+", false: "-"}[strings.Contains(string(v), "cache_control")]
		}
		return out
	}
	for _, c := range []struct{ model, url, system, messages, tools string }{
		{"a/claude-sonnet-4", "https://api.anthropic.com/v1/messages", "+", "-++", "-+"},
		{"or/anthropic/claude-sonnet-4", "https://openrouter.ai/api/v1/chat/completions", "", "+-++", "--"},
	} {
		m, err := reg.Resolve(c.model)
		if err != nil {
			t.Fatal(err)
		}
		r := NewRunner(m, tools, "sys")
		r.Hooks, r.ProviderOptions = newRecHooks(), reg.CallOptions(c.model, "", "")
		r.History = []fantasy.Message{UserMessage("a"), {Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "b"}}}}
		_ = r.Send(t.Context(), "c") // the server refuses; only the request matters
		mu.Lock()
		var req struct{ System, Messages, Tools []json.RawMessage }
		err = json.Unmarshal([]byte(body), &req)
		gotURL := sent
		mu.Unlock()
		if err != nil {
			t.Fatalf("%s: %v in %s", c.model, err, body)
		}
		got := [4]string{gotURL, marks(req.System), marks(req.Messages), marks(req.Tools)}
		if want := [4]string{c.url, c.system, c.messages, c.tools}; got != want {
			t.Fatalf("%s: url, system, messages, tools marks = %q, want %q", c.model, got, want)
		}
	}
}
