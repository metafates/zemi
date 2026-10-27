// ratelimit.go paces one provider's calls to the rate_limit its spec sets:
// a requests-per-window bucket and, when the spec names one, a
// completion-tokens-per-window bucket charged after each reply. Both are
// shared by everything calling the provider (the conversation, the builder,
// subagents), so a low-rate-limit provider serves the calls instead of
// refusing them. NOTE: per process; two zemi windows on one provider each
// pace their own half.

package agent

import (
	"context"
	"net/http"
	"sync"
	"time"

	"charm.land/fantasy"
)

// RateLimit is a provider's quota per window of Window seconds: Requests
// calls and, when CompletionTokens is set, that many reply tokens. The zero
// value paces nothing.
type RateLimit struct {
	Requests         int
	CompletionTokens int
	Window           int // seconds; 0 means 60
}

// normalize fills the defaults, so a limiter and its spec always agree.
func (r RateLimit) normalize() RateLimit {
	if r.Window <= 0 {
		r.Window = 60
	}
	return r
}

// Paced reports whether either quota is set.
func (r RateLimit) Paced() bool {
	return r.Requests > 0 || r.CompletionTokens > 0
}

type rateLimiter struct {
	spec RateLimit

	mu   sync.Mutex
	reqs bucket
	toks *bucket // nil without a completion-tokens quota
}

type bucket struct {
	perSec float64
	cap    float64
	tokens float64
	last   time.Time
}

func newRateLimiter(spec RateLimit) *rateLimiter {
	spec = spec.normalize()
	l := &rateLimiter{spec: spec, reqs: newBucket(spec.Requests, spec.Window)}
	if spec.CompletionTokens > 0 {
		b := newBucket(spec.CompletionTokens, spec.Window)
		l.toks = &b
	}
	return l
}

// newBucket makes a bucket of allow per windowSecs, full to start.
func newBucket(allow, windowSecs int) bucket {
	return bucket{perSec: float64(allow) / float64(windowSecs), cap: float64(allow), tokens: float64(allow), last: time.Now()}
}

func (b *bucket) refill(now time.Time) {
	b.tokens = min(b.cap, b.tokens+now.Sub(b.last).Seconds()*b.perSec)
	b.last = now
}

// wait holds until one request fits both quotas or ctx ends.
func (l *rateLimiter) wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := time.Now()
		if l.spec.Requests > 0 {
			l.reqs.refill(now)
		}
		d := time.Duration(0)
		if l.spec.Requests > 0 && l.reqs.tokens < 1 {
			d = time.Duration((1 - l.reqs.tokens) / l.reqs.perSec * float64(time.Second))
		} else if l.toks != nil {
			l.toks.refill(now)
			// a drained token budget holds the call: it would 429 otherwise
			if l.toks.tokens < 1 {
				d = time.Duration((1 - l.toks.tokens) / l.toks.perSec * float64(time.Second))
			}
		}
		if d <= 0 {
			l.reqs.tokens--
			l.mu.Unlock()
			return nil
		}
		l.mu.Unlock()
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// charge bills a reply's tokens against the completion budget; the bucket
// may go negative, so later calls wait out the drain instead of forcing a
// 429 the wait could have avoided.
func (l *rateLimiter) charge(tokens int64) {
	if l.toks == nil || tokens <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.toks.refill(time.Now())
	l.toks.tokens -= float64(tokens)
}

// rateModel is a provider's paced model. The wait sits outside idleModel,
// so the stall clock counts only a call the provider took; the reply's
// tokens charge the completion budget as they arrive.
type rateModel struct {
	fantasy.LanguageModel
	limiter *rateLimiter
}

func (m rateModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if err := m.limiter.wait(ctx); err != nil {
		return nil, err
	}
	resp, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	var seen int64
	return func(yield func(fantasy.StreamPart) bool) {
		for part := range resp {
			if part.Usage.OutputTokens > seen {
				m.limiter.charge(part.Usage.OutputTokens - seen)
				seen = part.Usage.OutputTokens
			}
			if !yield(part) {
				return
			}
		}
	}, nil
}

func (m rateModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	if err := m.limiter.wait(ctx); err != nil {
		return nil, err
	}
	resp, err := m.LanguageModel.Generate(ctx, call)
	if err != nil {
		return nil, err
	}
	m.limiter.charge(resp.Usage.OutputTokens)
	return resp, nil
}

func (m rateModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	if err := m.limiter.wait(ctx); err != nil {
		return nil, err
	}
	resp, err := m.LanguageModel.GenerateObject(ctx, call)
	if err != nil {
		return nil, err
	}
	m.limiter.charge(resp.Usage.OutputTokens)
	return resp, nil
}

func (m rateModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	if err := m.limiter.wait(ctx); err != nil {
		return nil, err
	}
	resp, err := m.LanguageModel.StreamObject(ctx, call)
	if err != nil {
		return nil, err
	}
	var seen int64
	return func(yield func(fantasy.ObjectStreamPart) bool) {
		for part := range resp {
			if part.Usage.OutputTokens > seen {
				m.limiter.charge(part.Usage.OutputTokens - seen)
				seen = part.Usage.OutputTokens
			}
			if !yield(part) {
				return
			}
		}
	}, nil
}

// PaceTransport wraps a transport in the limiter's request wait, so a
// raw-HTTP caller (classify's provider protocol) shares the pace the models
// go through. NOTE: its replies charge no completion budget - the protocol's
// JSON is read by internal/classify, not the model layer.
func PaceTransport(l *rateLimiter, base http.RoundTripper) http.RoundTripper {
	return paceTransport{base: base, limiter: l}
}

type paceTransport struct {
	base    http.RoundTripper
	limiter *rateLimiter
}

func (p paceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := p.limiter.wait(req.Context()); err != nil {
		return nil, err
	}
	return p.base.RoundTrip(req)
}
