package agent

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// TestRateLimiterWait: the bucket serves a burst at once, then paces at the
// minute's rate, and a caller whose context ends during the wait gets it.
func TestRateLimiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newRateLimiter(2) // 2 a minute: the burst, then one every 30s
		start := time.Now()
		for range 3 {
			if err := l.wait(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if d := time.Since(start); d != 30*time.Second {
			t.Fatalf("3 calls took %v, want the burst at once then one pace", d)
		}
	})
}

func TestRateLimiterCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newRateLimiter(1) // one a minute
		if err := l.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- l.wait(ctx) }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled wait = %v, want context.Canceled", err)
		}
	})
}
