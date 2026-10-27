// stub_test.go is the side-call-aware model stub the chat flow tests share.

package hostapi

import (
	"context"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/store"
)

// replyStubModel answers side calls through Generate and turns through
// Stream with a fixed text (Stream records the prompt size); object calls
// refuse.
type replyStubModel struct {
	resp string
	err  error
	// how long a call takes, in ns: a race-free read, since a title call
	// and a turn's calls run at once
	block      atomic.Int64
	calls      atomic.Int32
	lastPrompt atomic.Int32
	lastOpts   atomic.Pointer[fantasy.ProviderOptions] // the last Generate call's provider options
	streams    atomic.Int32
	overflow   atomic.Int32                          // Stream calls left that fail as context too large
	toolStep   atomic.Int64                          // context the next Stream call reports along with a tool call; 0 = text
	failOnce   atomic.Pointer[fantasy.ProviderError] // the next Stream call's error
	cutReply   bool                                  // Generate stops at the output limit
	during     func()                                // runs inside Generate, while the model "thinks"
	mu         sync.Mutex
	prompts    [][]fantasy.Message // every Stream call's prompt but a side call's
	tools      [][]fantasy.Tool    // and its tools
}

func (m *replyStubModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.calls.Add(1)
	m.lastOpts.Store(&call.ProviderOptions)
	if m.during != nil {
		m.during()
	}
	if d := time.Duration(m.block.Load()); d > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(d):
		}
	}
	if m.err != nil {
		return nil, m.err
	}
	finish := fantasy.FinishReasonStop
	if m.cutReply {
		finish = fantasy.FinishReasonLength
	}
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: m.resp}},
		FinishReason: finish,
		Usage:        fantasy.Usage{OutputTokens: 7},
	}, nil
}

func (m *replyStubModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if sideCall(call) {
		res, err := m.Generate(ctx, call)
		if err != nil {
			return nil, err
		}
		return streamOf(res), nil
	}
	m.streams.Add(1)
	m.lastPrompt.Store(int32(len(call.Prompt)))
	m.mu.Lock()
	m.prompts = append(m.prompts, call.Prompt)
	m.tools = append(m.tools, call.Tools)
	m.mu.Unlock()
	if used := m.toolStep.Swap(0); used > 0 {
		return func(yield func(fantasy.StreamPart) bool) {
			_ = yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "c1", ToolCallName: "nope", ToolCallInput: "{}"}) &&
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls, Usage: fantasy.Usage{InputTokens: used}})
		}, nil
	}
	if m.overflow.Add(-1) >= 0 {
		return nil, &fantasy.ProviderError{Message: "prompt is too long", ContextTooLargeErr: true}
	}
	if pe := m.failOnce.Swap(nil); pe != nil {
		return nil, pe
	}
	return slices.Values(textParts(m.resp)), nil
}

// sideCall reports a builder or recap call, which a turn's script does not
// answer.
func sideCall(call fantasy.Call) bool {
	return len(call.Prompt) > 0 && (slices.Contains([]string{chatSystem}, concatText(call.Prompt[0].Content)) ||
		concatText(call.Prompt[len(call.Prompt)-1].Content) == recapPrompt)
}

// streamOf replays a reply as the stream a side call reads.
func streamOf(res *fantasy.Response) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		for _, c := range res.Content {
			switch c := c.(type) {
			case fantasy.TextContent:
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "t"}) ||
					!yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t", Delta: c.Text}) ||
					!yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "t"}) {
					return
				}
			case fantasy.ToolCallContent:
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: c.ToolCallID, ToolCallName: c.ToolName, ToolCallInput: c.Input}) {
					return
				}
			}
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: res.FinishReason, Usage: res.Usage})
	}
}

// replyStub installs m as the model every call resolves to.
func replyStub(t *testing.T, h *Host, resp string) *replyStubModel {
	t.Helper()
	m := &replyStubModel{resp: resp}
	h.modelFn = func() (fantasy.LanguageModel, error) { return m, nil }
	return m
}

func (m *replyStubModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *replyStubModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *replyStubModel) Provider() string { return "stub" }
func (m *replyStubModel) Model() string    { return "stub-1" }

// sender sends one message and waits for its turn to settle.
func sender(t *testing.T, h *Host) func(string) {
	return func(text string) {
		t.Helper()
		if err := h.AgentSend(text); err != nil {
			t.Fatalf("send %q: %v", text, err)
		}
		waitIdle(t, h)
	}
}

// seedSession names the session the chat bills to: the anchor row, which the
// migration plants, so there is nothing to seed.
func seedSession(t *testing.T, h *Host) string {
	t.Helper()
	if h.AgentSessionID() == "" {
		t.Fatal("no anchor session")
	}
	return h.AgentSessionID()
}

// waitIdle waits for the live turn to finish.
func waitIdle(t *testing.T, h *Host) {
	t.Helper()
	waitFor(t, func() error {
		if h.AgentActive() {
			return errors.New("turn did not finish")
		}
		return nil
	})
}

// addText stores a run transcript row the way the run's Persist does.
func addText(t *testing.T, h *Host, sid, role, text string) {
	t.Helper()
	parts, err := json.Marshal(fantasy.Message{Role: fantasy.MessageRole(role), Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}}}, json.Deterministic(true))
	if err == nil {
		err = h.DB.AddMessage(&store.Message{SessionID: sid, Role: role, PartsJSON: string(parts)})
	}
	if err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
}

// lastUserText is what the model last read of the user's words: the log's
// newest user row, then the attached text it rode in with. The attach echo
// is logged as the message composes, before its user row, so the echoes
// between the two newest user rows are this send's.
func lastUserText(t *testing.T, h *Host) string {
	t.Helper()
	rows, err := h.DB.LogKindBack(kindUser, 2)
	if err != nil || len(rows) == 0 {
		t.Fatalf("no user row logged: %v", err)
	}
	last, prev := rows[len(rows)-1], int64(-1)
	if len(rows) == 2 {
		prev = rows[0].Seq
	}
	text := last.Text
	attached, _ := h.DB.LogKindBack(kindEcho, 50)
	for _, row := range attached {
		if rowTool(row) == "attach" && row.Seq > prev && row.Seq < last.Seq {
			text += agent.AttachMarker + row.Text
		}
	}
	return text
}

// toolResult is one tool result message of about tokens.
func toolResult(id string, tokens int) fantasy.Message {
	return fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
		fantasy.ToolResultPart{ToolCallID: id, Output: fantasy.ToolResultOutputContentText{Text: strings.Repeat("r", tokens*4)}},
	}}
}

// estTokens guesses the size of messages at 4 bytes a token, 1k per file.
func estTokens(msgs ...fantasy.Message) int64 {
	var n int64
	for _, m := range msgs {
		for _, part := range m.Content {
			switch p := part.(type) {
			case fantasy.TextPart:
				n += int64(len(p.Text))
			case fantasy.ReasoningPart:
				n += int64(len(p.Text))
			case fantasy.ToolCallPart:
				n += int64(len(p.Input))
			case fantasy.ToolResultPart:
				n += int64(len(toolText(p)))
			case fantasy.FilePart:
				n += 4_000
			}
		}
	}
	return n / 4
}

func toolText(p fantasy.ToolResultPart) string {
	if p.Output == nil {
		return ""
	}
	txt, _ := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](p.Output)
	return txt.Text
}
