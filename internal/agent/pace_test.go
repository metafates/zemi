package agent

import (
	"net/http"
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
