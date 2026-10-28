package hostapi

// A turn cancelled mid-reply still flushed its partial text to the
// transcript, but the interrupted step's log write is gone with
// OnStepFinish - nothing will name those rows. The pairing cursor must
// consume them at the dead turn's end, or it sits on the orphan and every
// later row of the window stays unnamed (zoom's target lost) until a reset.

import (
	"context"
	"errors"
	"slices"
	"testing"

	"charm.land/fantasy"
)

type slowModel struct{ release chan struct{} }

func (m *slowModel) StreamInference(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "t"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t", Delta: "partial reply text"}) {
			return
		}
		select {
		case <-ctx.Done():
		case <-m.release:
		}
	}, nil
}

func (m *slowModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return m.StreamInference(ctx, fantasy.Call{})
}

func (m *slowModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("no")
}

func (m *slowModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("no")
}

func (m *slowModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("no")
}
func (m *slowModel) Provider() string { return "slow" }
func (m *slowModel) Model() string    { return "slow-1" }

func TestCancelPairsTheNextTurn(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.SetUIHooks(&fakeUI{})
	m := &slowModel{release: make(chan struct{})}
	h.modelFn = func() (fantasy.LanguageModel, error) { return m, nil }

	if err := h.AgentSend("q1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if !h.AgentActive() {
			return errors.New("turn not active yet")
		}
		return nil
	})
	h.AgentCancel()
	waitIdle(t, h)

	tr := h.Session().Transcript
	orphan := 0
	for _, row := range tr {
		if row.Role == fantasy.MessageRoleAssistant && row.Seq == NoSeq {
			orphan++
		}
	}
	if orphan == 0 {
		t.Fatalf("no unstamped reply row after a mid-stream cancel; rows=%v", tr)
	}

	close(m.release)
	if err := h.AgentSend("q2"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	tr = h.Session().Transcript
	q2 := slices.IndexFunc(tr, func(m ChatMessage) bool { return m.Role == fantasy.MessageRoleUser && m.Text == "q2" })
	if q2 < 0 {
		t.Fatal("q2 row missing")
	}
	if tr[q2].Seq == NoSeq {
		t.Errorf("q2 never pairs after a mid-stream cancel: the flushed reply tail jammed the pairing")
	}
}
