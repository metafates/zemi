package lua

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	glua "github.com/yuin/gopher-lua"
)

// Class is a request priority class (DESIGN §4): the queue drains ui
// first, then agent, then io; FIFO within a class.
type Class int

const (
	ClassUI Class = iota
	ClassAgent
	ClassIO
)

func (c Class) String() string {
	switch c {
	case ClassUI:
		return "ui"
	case ClassAgent:
		return "agent"
	default:
		return "io"
	}
}

// Timing budgets (DESIGN §4).
const (
	// SoftBudget is the soft handler budget; exceeding it logs a warning,
	// it does not preempt.
	SoftBudget = 10 * time.Millisecond
	// HardCeiling is the hard ceiling for ui/io handlers.
	HardCeiling = time.Second
	// AbandonGrace is how long a stuck request may keep running after the
	// ceiling fired before the runtime abandons it and goes hot.
	AbandonGrace = 10 * time.Second
	// VetoDeadline bounds a single vetoable event handler.
	VetoDeadline = 50 * time.Millisecond
)

// Sentinel errors surfaced to callers.
var (
	// ErrHot means the runtime is wedged and needs :Reload.
	ErrHot = errors.New("zemi: lua runtime is hot, reload required")
	// ErrReloaded means the request's generation was replaced.
	ErrReloaded = errors.New("zemi: lua runtime reloaded, request dropped")
	// ErrAbandoned means the request exceeded ceiling+grace and the
	// interpreter was left spinning (DESIGN §4).
	ErrAbandoned = errors.New("zemi: lua request abandoned after grace period")
	// ErrCanceled is delivered when the request context was canceled.
	ErrCanceled = context.Canceled
)

// Fn runs on the interpreter goroutine. Return values are marshaled to the
// waiting caller; a non-nil error fails the request.
type Fn func(L *glua.LState) ([]any, error)

type request struct {
	class   Class
	fn      Fn
	caller  context.Context
	reply   chan result
	ceiling time.Duration
	grace   time.Duration
	soft    time.Duration

	cancelFn context.CancelFunc
	mu       sync.Mutex
	finished bool // done or abandoned
	done     chan struct{}
	seq      uint64
}

type result struct {
	vals     []any
	err      error
	canceled bool
}

func newRequest(class Class, fn Fn, ctx context.Context, ceiling, grace, soft time.Duration) *request {
	return &request{
		class:   class,
		fn:      fn,
		caller:  ctx,
		reply:   make(chan result, 1),
		ceiling: ceiling,
		grace:   grace,
		soft:    soft,
		done:    make(chan struct{}),
	}
}

// generation is one interpreter instance: an LState, its queue and its
// dispatcher goroutine. A wedged generation is dropped wholesale; the
// replacement boots fresh (DESIGN §4 hot mode).
type Generation struct {
	ID      int64
	rt      *Runtime
	SB      *Sandbox
	L       *glua.LState
	modules map[string]glua.LValue

	mu      sync.Mutex
	heap    []*request
	seq     uint64
	signal  chan struct{}
	retired bool
}

func (g *Generation) enqueue(r *request) {
	g.mu.Lock()
	if g.retired {
		g.mu.Unlock()
		r.reply <- result{err: ErrReloaded}
		return
	}
	g.seq++
	r.seq = g.seq
	g.heap = append(g.heap, r)
	g.mu.Unlock()
	select {
	case g.signal <- struct{}{}:
	default:
	}
}

func (g *Generation) pop() *request {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.heap) == 0 {
		return nil
	}
	best := 0
	for i, r := range g.heap {
		if r.class < g.heap[best].class ||
			(r.class == g.heap[best].class && r.seq < g.heap[best].seq) {
			best = i
		}
	}
	r := g.heap[best]
	g.heap[best] = nil
	g.heap = append(g.heap[:best], g.heap[best+1:]...)
	return r
}

func (g *Generation) drain(retire bool) {
	g.mu.Lock()
	g.retired = retire
	pending := g.heap
	g.heap = nil
	g.mu.Unlock()
	for _, r := range pending {
		r.mu.Lock()
		if !r.finished {
			r.finished = true
			close(r.done)
			r.reply <- result{err: ErrReloaded}
		}
		r.mu.Unlock()
	}
}

// loop is the interpreter goroutine: it never touches the LState outside
// popped requests and enforces per-request cancellation and budgets.
func (g *Generation) loop() {
	for {
		r := g.pop()
		if r == nil {
			select {
			case <-g.signal:
				continue
			case <-g.rt.retireCh(g.ID):
				return
			}
		}
		g.execute(r)
	}
}

func (g *Generation) execute(r *request) {
	g.mu.Lock()
	retired := g.retired
	g.mu.Unlock()
	if retired {
		r.reply <- result{err: ErrReloaded}
		return
	}

	reqCtx, cancel := context.WithCancel(r.caller)
	r.cancelFn = cancel
	ceiling := r.ceiling
	if ceiling <= 0 {
		ceiling = HardCeiling
	}
	grace := r.grace
	if grace <= 0 {
		grace = AbandonGrace
	}
	soft := r.soft
	if soft <= 0 {
		soft = SoftBudget
	}

	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		slow := time.AfterFunc(soft, func() {
			g.rt.log.Warn("lua", "handler exceeded soft budget", map[string]any{
				"class": r.class.String(), "budget": soft.String(),
			})
		})
		defer slow.Stop()
		hard := time.AfterFunc(ceiling, cancel)
		defer hard.Stop()
		select {
		case <-r.done:
		case <-time.After(ceiling + grace):
			if g.finish(r) {
				cancel()
				// Mark hot BEFORE answering: while hot every Lua request
				// fails fast, so a request arriving after the abandon
				// cannot enqueue onto the wedged interpreter.
				g.rt.goHot(fmt.Errorf("request in class %s exceeded ceiling %s + grace %s",
					r.class.String(), ceiling, grace))
				r.reply <- result{err: ErrAbandoned, canceled: true}
			}
		}
	}()
	defer close(r.done)

	started := time.Now()
	g.L.SetContext(reqCtx)
	g.rt.PushFrame(OriginHost, "")
	vals, err := g.safeRun(r)
	g.rt.PopFrame()
	g.L.RemoveContext()

	elapsed := time.Since(started)
	if err == nil && elapsed > soft && reqCtx.Err() == nil {
		g.rt.log.Debug("lua", "slow handler completed", map[string]any{
			"class": r.class.String(), "elapsed": elapsed.String(),
		})
	}

	if r.isFinished() {
		// The watchdog abandoned this request while we were still running;
		// the runtime is hot and the reply already carries ErrAbandoned.
		cancel()
		return
	}
	if reqCtx.Err() != nil {
		// A reply arriving after cancellation is canceled, whatever the Lua
		// returned (DESIGN §4).
		g.finish(r)
		r.reply <- result{vals: vals, err: fmt.Errorf("%w: %v", ErrCanceled, reqCtx.Err()), canceled: true}
		cancel()
		return
	}
	g.finish(r)
	if err != nil {
		r.reply <- result{err: err}
	} else {
		r.reply <- result{vals: vals}
	}
	cancel()
}

// finish marks the request finished; it returns false if the watchdog
// already abandoned it.
func (g *Generation) finish(r *request) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return false
	}
	r.finished = true
	return true
}

func (r *request) isFinished() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finished
}

func (g *Generation) safeRun(r *request) (vals []any, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("zemi bug: lua host function panicked: %v", rec)
			g.rt.log.Error("lua", err.Error(), nil)
		}
	}()
	return r.fn(g.L)
}
