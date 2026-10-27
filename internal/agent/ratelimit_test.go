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

// TestLimiterRidesTheSpec: a re-register that changes or drops rate_limit
// replaces or drops the pace, so a config reload cannot keep pacing at the
// old rate through a stale limiter.
func TestLimiterRidesTheSpec(t *testing.T) {
	r := &Registry{}
	spec := func(rate int) Spec {
		return Spec{ID: "gw", Kind: KindOpenAICompat, BaseURL: "http://127.0.0.1:1/v1", RateLimit: rate, Models: []Model{{ID: "m"}}}
	}
	paced := func() float64 {
		m, err := r.Resolve("gw/m")
		if err != nil {
			t.Fatal(err)
		}
		rm, is := m.(rateModel)
		if !is {
			return 0
		}
		return rm.limiter.perMin
	}
	if err := r.Register(spec(6), "k", false); err != nil {
		t.Fatal(err)
	}
	if got := paced(); got != 6 {
		t.Fatalf("pace = %v, want 6", got)
	}
	if err := r.Register(spec(60), "k", false); err != nil {
		t.Fatal(err)
	}
	if got := paced(); got != 60 {
		t.Fatalf("pace after the re-register = %v, want 60", got)
	}
	if err := r.Register(spec(0), "k", false); err != nil {
		t.Fatal(err)
	}
	if got := paced(); got != 0 {
		t.Fatalf("a dropped rate_limit still paces at %v, want unpaced", got)
	}
}
