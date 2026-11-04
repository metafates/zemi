// ratelimit.go paces one provider's calls to the rate_limit its spec sets:
// a requests-per-window bucket and, when the spec names one, a
// completion-tokens-per-window bucket charged after each reply. Both are
// shared by everything calling the provider (the conversation, the builder,
// subagents), so a low-rate-limit provider serves the calls instead of
// refusing them. The buckets' state lives in <state>/pace, shared with the
// other zemi processes of the user (pace.go), so parallel windows pace the
// provider's one quota together; a process that cannot read the file, or
// one on another state home, paces its own.

package agent

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"strconv"
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
	spec   RateLimit
	shared *paceFile // set when the pace is shared with the other zemi processes

	mu    sync.Mutex
	state paceState
}

// paceState is a limiter's mutable pacing state: in the limiter's memory, or
// loaded fresh from the shared file on every access.
type paceState struct {
	last time.Time // the requests metronome's last admission
	toks *bucket   // nil without a completion-tokens quota
	hold time.Time // no call starts before it: a 429 taught the pace the window is spent
}

type bucket struct {
	perSec float64
	cap    float64
	tokens float64
	last   time.Time
}

func newRateLimiter(spec RateLimit, shared *paceFile) *rateLimiter {
	spec = spec.normalize()
	l := &rateLimiter{spec: spec, shared: shared}
	l.state = *l.freshState()
	return l
}

// freshState builds the state a limiter starts from: the metronome unset,
// the token bucket full.
func (l *rateLimiter) freshState() *paceState {
	st := &paceState{}
	if l.spec.CompletionTokens > 0 {
		b := newBucket(l.spec.CompletionTokens, l.spec.Window)
		st.toks = &b
	}
	return st
}

// newBucket makes a bucket of allow per windowSecs, full to start.
func newBucket(allow, windowSecs int) bucket {
	return bucket{perSec: float64(allow) / float64(windowSecs), cap: float64(allow), tokens: float64(allow), last: time.Now()}
}

func (b *bucket) refill(now time.Time) {
	b.tokens = min(b.cap, b.tokens+now.Sub(b.last).Seconds()*b.perSec)
	b.last = now
}

// take admits one request when the state allows it, answering how long a
// retry should wait; 0 means the request was taken.
func (st *paceState) take(spec RateLimit) time.Duration {
	now := time.Now()
	if now.Before(st.hold) {
		return st.hold.Sub(now)
	}
	if spec.Requests > 0 {
		// a metronome, not a burst: the window's requests spread evenly, so
		// the quota's own pace never empties a rolling window and then
		// refills into refusals the way a full-to-start bucket would
		if d := st.last.Add(time.Duration(float64(spec.Window) / float64(spec.Requests) * float64(time.Second))).Sub(now); d > 0 {
			return d
		}
		st.last = now
	}
	if st.toks != nil {
		st.toks.refill(now)
		// a drained token budget holds the call: it would 429 otherwise
		if st.toks.tokens < 1 {
			return time.Duration((1 - st.toks.tokens) / st.toks.perSec * float64(time.Second))
		}
	}
	return 0
}

// wait holds until one request fits both quotas and no refusal hold is
// armed, or ctx ends.
func (l *rateLimiter) wait(ctx context.Context) error {
	for {
		d := l.take()
		if d == 0 {
			return nil
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// take runs one admission against the memory state, or the shared file's.
func (l *rateLimiter) take() time.Duration {
	if l.shared == nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.state.take(l.spec)
	}
	var d time.Duration
	l.shared.update(l, func(st *paceState) { d = st.take(l.spec) })
	return d
}

// charge bills a reply's tokens against the completion budget; the bucket
// may go negative, so later calls wait out the drain instead of forcing a
// 429 the wait could have avoided.
func (l *rateLimiter) charge(tokens int64) {
	if tokens <= 0 {
		return
	}
	mutate := func(st *paceState) {
		if st.toks == nil {
			return
		}
		st.toks.refill(time.Now())
		st.toks.tokens -= float64(tokens)
	}
	if l.shared == nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		mutate(&l.state)
		return
	}
	l.shared.update(l, mutate)
}

// refused teaches the pace from a gateway refusal: a 429 arms the hold, so
// the retries fantasy spaces seconds apart land after the reset instead of
// spending the window's remaining quota on refusals.
func (l *rateLimiter) refused(err error) {
	if pe, ok := errors.AsType[*fantasy.ProviderError](err); ok && pe.StatusCode == http.StatusTooManyRequests {
		// fantasy keeps the header's own case in ProviderError
		l.hold429(pe.ResponseHeaders["retry-after-ms"], pe.ResponseHeaders["retry-after"],
			pe.ResponseHeaders["Retry-After-Ms"], pe.ResponseHeaders["Retry-After"])
	}
}

// hold429 arms the hold until the refusal says the window rolls: its
// Retry-After when it names one, the spec's window when it does not. A
// nonsense or over-long header falls back to the window too.
func (l *rateLimiter) hold429(retryAfterMs, retryAfter, retryAfterMsUp, retryAfterUp string) {
	d := time.Duration(l.spec.Window) * time.Second
	if v := cmp.Or(retryAfterMs, retryAfterMsUp); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 {
			d = time.Duration(ms * float64(time.Millisecond))
		}
	} else if v := cmp.Or(retryAfter, retryAfterUp); v != "" {
		if s, err := strconv.ParseFloat(v, 64); err == nil && s > 0 {
			d = time.Duration(s * float64(time.Second))
		} else if t, err := http.ParseTime(v); err == nil && time.Until(t) > 0 {
			d = time.Until(t)
		}
	}
	if d <= 0 {
		d = time.Duration(l.spec.Window) * time.Second
	} else if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	mutate := func(st *paceState) {
		if until := time.Now().Add(d); until.After(st.hold) {
			st.hold = until
		}
	}
	if l.shared == nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		mutate(&l.state)
		return
	}
	l.shared.update(l, mutate)
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
		m.limiter.refused(err)
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
		m.limiter.refused(err)
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
		m.limiter.refused(err)
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
		m.limiter.refused(err)
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
	resp, err := p.base.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusTooManyRequests {
		// the protocol reads the body itself; the pace learns from the headers
		p.limiter.hold429(resp.Header.Get("Retry-After-Ms"), resp.Header.Get("Retry-After"), "", "")
	}
	return resp, err
}
