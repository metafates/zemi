package agent

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/fantasy"
)

// streamStub streams whatever stream does with the context it is given.
type streamStub struct {
	fantasy.LanguageModel

	stream func(ctx context.Context, yield func(fantasy.StreamPart) bool)
}

func (s streamStub) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) { s.stream(ctx, yield) }, nil
}

func delta(text string) fantasy.StreamPart {
	return fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: text}
}

func TestIdleModelCutsAStalledStream(t *testing.T) {
	const idle = 50 * time.Millisecond
	for _, c := range []struct {
		name    string
		stream  func(ctx context.Context, yield func(fantasy.StreamPart) bool)
		wantErr bool
	}{
		{"silent after a chunk", func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
			yield(delta("hi"))
			<-ctx.Done()
		}, true},
		{"reports the cancel", func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
			yield(delta("hi"))
			<-ctx.Done()
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
		}, true},
		{"slow but alive", func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
			for range 4 {
				time.Sleep(idle / 2)
				if !yield(delta("hi")) {
					return
				}
			}
		}, false},
	} {
		// a fake clock: a starved machine cannot stretch a sleep past idle
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := idleModel{LanguageModel: streamStub{stream: c.stream}, idle: idle}
				seq, err := m.Stream(t.Context(), fantasy.Call{})
				if err != nil {
					t.Fatal(err)
				}
				var last fantasy.StreamPart
				n := 0
				done := make(chan struct{})
				go func() {
					defer close(done)
					for p := range seq {
						last, n = p, n+1
					}
				}()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("the stream never ended")
				}
				gotErr := errors.Is(last.Error, errModelIdle)
				if gotErr != c.wantErr {
					t.Fatalf("last part %+v (%d parts), want idle error: %v", last, n, c.wantErr)
				}
			})
		})
	}
}
