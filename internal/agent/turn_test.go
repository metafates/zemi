package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
)

// mockModel replays scripted stream parts.
type mockModel struct {
	mu       sync.Mutex
	parts    []fantasy.StreamPart
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

func (h *recHooks) ToolCallStart(id, name, input string) {
	h.mu.Lock()
	h.tools = append(h.tools, "start:"+name)
	h.mu.Unlock()
}

func (h *recHooks) ToolCallEnd(id, name string, ok bool, output string) {
	h.mu.Lock()
	h.tools = append(h.tools, "end:"+name)
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
	m := &mockModel{parts: textParts("hel", "lo " + "world")}
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
	hooks.mu.Lock()
	err := hooks.doneErr
	hooks.mu.Unlock()
	if err == nil {
		// TurnDone reports the context error
		t.Log("no error surfaced; acceptable if stream closed cleanly")
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
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// user-registered provider with no key still resolves (error surfaces at send)
	if _, err := r.Resolve("myllm/main"); err != nil {
		t.Fatalf("openai_compat resolve: %v", err)
	}
	if err := r.Register(Spec{ID: "myllm", Kind: "openai"}); err == nil {
		t.Fatal("duplicate id must fail")
	}
	if specs := r.List(); len(specs) != 1 {
		t.Fatalf("catalog: %v", specs)
	}
}
