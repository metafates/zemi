package agent

import (
	"net/http"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/fantasy"
)

// pacePair builds two limiters on one shared pace file, the shape of two
// zemi processes on one provider.
func pacePair(t *testing.T, rate RateLimit) (*rateLimiter, *rateLimiter) {
	t.Helper()
	dir := t.TempDir()
	a := newRateLimiter(rate, newPaceFile(dir, "key", "prov"))
	b := newRateLimiter(rate, newPaceFile(dir, "key", "prov"))
	return a, b
}

// TestSharedPaceSplitsTheQuota: two processes drawing on one file share the
// metronome - one admits, the other waits for the next even slot.
func TestSharedPaceSplitsTheQuota(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := pacePair(t, RateLimit{Requests: 2})
		if d := a.take(); d != 0 {
			t.Fatalf("a's first take waited %v", d)
		}
		if d := b.take(); d != 30*time.Second {
			t.Fatalf("b's take waited %v, want the shared metronome's next slot", d)
		}
		if d := a.take(); d != 30*time.Second {
			t.Fatalf("a's next take waited %v, want the shared slot after b's", d)
		}
	})
}

// TestSharedPaceCarriesTheHold: a refusal one process saw holds the other's
// calls too, so parallel windows back off together.
func TestSharedPaceCarriesTheHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := pacePair(t, RateLimit{Requests: 100})
		a.refused(&fantasy.ProviderError{
			StatusCode:      http.StatusTooManyRequests,
			ResponseHeaders: map[string]string{"Retry-After": "5"},
		})
		if d := b.take(); d != 5*time.Second {
			t.Fatalf("b's take waited %v, want the shared hold's 5s", d)
		}
	})
}

// TestSharedPaceCarriesTheCharge: one process's reply tokens drain the
// shared completion budget for the other too.
func TestSharedPaceCarriesTheCharge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := pacePair(t, RateLimit{CompletionTokens: 10})
		a.charge(11) // one over the budget: the shared bucket sinks to -1
		if d := b.take(); d != 12*time.Second {
			t.Fatalf("b's take waited %v, want the shared debt's 12s", d)
		}
	})
}

// TestSharedPaceSurvivesACorruptFile: a state nothing can read starts fresh
// again, so a broken file degrades to per-process pacing, never to a stop.
func TestSharedPaceSurvivesACorruptFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		a := newRateLimiter(RateLimit{Requests: 2}, newPaceFile(dir, "key", "prov"))
		b := newRateLimiter(RateLimit{Requests: 2}, newPaceFile(dir, "key", "prov"))
		if d := a.take(); d != 0 {
			t.Fatalf("a's first take waited %v", d)
		}
		if err := os.WriteFile(newPaceFile(dir, "key", "prov").path, []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if d := b.take(); d != 0 {
			t.Fatalf("b's take over a corrupt file waited %v, want a fresh metronome", d)
		}
	})
}
