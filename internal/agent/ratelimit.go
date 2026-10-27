// ratelimit.go paces one provider's calls to the rate_limit its spec sets:
// a token bucket shared by everything calling the provider (the
// conversation, the builder, subagents), burst the minute's budget and
// refill at the same rate, so a low-rate-limit provider serves the calls
// instead of refusing them. NOTE: per process; two zemi windows on one
// provider each pace their own half.

package agent

import (
	"context"
	"sync"
	"time"

	"charm.land/fantasy"
)

type rateLimiter struct {
	perMin float64

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newRateLimiter(perMin int) *rateLimiter {
	return &rateLimiter{perMin: float64(perMin), tokens: float64(perMin), last: time.Now()}
}

// wait holds until one request fits the rate or ctx ends.
func (l *rateLimiter) wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens = min(l.perMin, l.tokens+now.Sub(l.last).Minutes()*l.perMin)
		l.last = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		d := time.Duration((1 - l.tokens) / l.perMin * float64(time.Minute))
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

// rateModel is a provider's paced model. The wait sits outside idleModel,
// so the stall clock counts only a call the provider took.
type rateModel struct {
	fantasy.LanguageModel
	limiter *rateLimiter
}

func (m rateModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if err := m.limiter.wait(ctx); err != nil {
		return nil, err
	}
	return m.LanguageModel.Stream(ctx, call)
}

func (m rateModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	if err := m.limiter.wait(ctx); err != nil {
		return nil, err
	}
	return m.LanguageModel.Generate(ctx, call)
}

func (m rateModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	if err := m.limiter.wait(ctx); err != nil {
		return nil, err
	}
	return m.LanguageModel.GenerateObject(ctx, call)
}

func (m rateModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	if err := m.limiter.wait(ctx); err != nil {
		return nil, err
	}
	return m.LanguageModel.StreamObject(ctx, call)
}
