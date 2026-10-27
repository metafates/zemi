package agent

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/fantasy"
)

// TestRateLimiterWait: the metronome spreads the window's requests evenly -
// one at once, then one every window/allow - and a caller whose context ends
// during the wait gets it.
func TestRateLimiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newRateLimiter(RateLimit{Requests: 2}, nil) // 2 a minute: one every 30s
		start := time.Now()
		for range 3 {
			if err := l.wait(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if d := time.Since(start); d != 60*time.Second {
			t.Fatalf("3 calls took %v, want the first at once then two paces", d)
		}
	})
}

func TestRateLimiterCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newRateLimiter(RateLimit{Requests: 1}, nil) // one a minute
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

// TestRateLimiterTokens: a drained completion budget holds the next call
// until a token refills, and the reply's charge may sink it negative.
func TestRateLimiterTokens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newRateLimiter(RateLimit{CompletionTokens: 10}, nil)
		if err := l.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		l.charge(11) // one over the window's budget: the bucket sinks to -1
		start := time.Now()
		if err := l.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d != 12*time.Second {
			t.Fatalf("the drained budget held the call %v, want the debt's 12s", d)
		}
	})
}

// TestRateLimiterTokensOnly: a completion budget alone paces without a
// request quota.
func TestRateLimiterTokensOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newRateLimiter(RateLimit{CompletionTokens: 10}, nil)
		start := time.Now()
		for range 3 {
			if err := l.wait(context.Background()); err != nil {
				t.Fatal(err)
			}
			l.charge(10)
		}
		if d := time.Since(start); d < 12*time.Second {
			t.Fatalf("3 drained calls took %v, want each to wait a token out", d)
		}
	})
}

// TestRateLimiterHold: a 429 through the limiter holds the next calls for
// the spec's window, so fantasy's retries land after the reset instead of
// spending the window's quota on refusals; anything but a 429 teaches
// nothing.
func TestRateLimiterHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newRateLimiter(RateLimit{Requests: 100}, nil)
		l.refused(errors.New("plain failure"))
		l.refused(&fantasy.ProviderError{StatusCode: http.StatusForbidden})
		start := time.Now()
		if err := l.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d != 0 {
			t.Fatalf("a non-429 held the call %v, want none", d)
		}
		l.refused(&fantasy.ProviderError{StatusCode: http.StatusTooManyRequests})
		start = time.Now()
		if err := l.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d != 60*time.Second {
			t.Fatalf("the refusal held the call %v, want the window's 60s", d)
		}
	})
}

// TestRateLimiterHoldRidesRetryAfter: the refusal's own Retry-After sets the
// hold, seconds or HTTP date, and one asking a decade clamps to ten minutes.
func TestRateLimiterHoldRidesRetryAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newRateLimiter(RateLimit{Requests: 100}, nil)
		l.hold429("250", "", "", "")
		start := time.Now()
		if err := l.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d != 250*time.Millisecond {
			t.Fatalf("the hold lasted %v, want retry-after-ms's 250ms", d)
		}
		l.hold429("", "7", "", "")
		start = time.Now()
		if err := l.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d != 7*time.Second {
			t.Fatalf("the hold lasted %v, want retry-after's 7s", d)
		}
		l.hold429("", "99999", "", "")
		start = time.Now()
		if err := l.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d != 10*time.Minute {
			t.Fatalf("the hold lasted %v, want the decade clamped to ten minutes", d)
		}
	})
}

// TestLimiterRidesTheSpec: a re-register that changes or drops rate_limit
// replaces or drops the pace, so a config reload cannot keep pacing at the
// old rate through a stale limiter.
func TestLimiterRidesTheSpec(t *testing.T) {
	r := &Registry{}
	spec := func(rate RateLimit) Spec {
		return Spec{ID: "gw", Kind: KindOpenAICompat, BaseURL: "http://127.0.0.1:1/v1", RateLimit: rate, Models: []Model{{ID: "m"}}}
	}
	paced := func() RateLimit {
		m, err := r.Resolve("gw/m")
		if err != nil {
			t.Fatal(err)
		}
		rm, is := m.(rateModel)
		if !is {
			return RateLimit{}
		}
		return rm.limiter.spec
	}
	if err := r.Register(spec(RateLimit{Requests: 6}), "k", false); err != nil {
		t.Fatal(err)
	}
	if got := paced(); got != (RateLimit{Requests: 6, Window: 60}) {
		t.Fatalf("pace = %+v, want 6 requests a minute", got)
	}
	if err := r.Register(spec(RateLimit{Requests: 60, CompletionTokens: 52000}), "k", false); err != nil {
		t.Fatal(err)
	}
	if got := paced(); got != (RateLimit{Requests: 60, CompletionTokens: 52000, Window: 60}) {
		t.Fatalf("pace after the re-register = %+v, want both quotas", got)
	}
	if err := r.Register(spec(RateLimit{}), "k", false); err != nil {
		t.Fatal(err)
	}
	if got := paced(); got.Paced() {
		t.Fatalf("a dropped rate_limit still paces at %+v, want unpaced", got)
	}
}
