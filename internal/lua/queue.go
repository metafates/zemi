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
	// ErrHot means the runtime is wedged and needs /reload.
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

	// cancelFn is installed by the executor goroutine and read by the
	// waiting caller's, so mu guards it like finished.
	mu       sync.Mutex
	cancelFn context.CancelFunc
	finished bool // done or abandoned
	done     chan struct{}
	seq      uint64
}

func (r *request) setCancel(fn context.CancelFunc) {
	r.mu.Lock()
	r.cancelFn = fn
	r.mu.Unlock()
}

// cancel preempts the request if it is already running; a request still
// waiting in the heap has no cancel func yet and settles on drain.
func (r *request) cancel() {
	r.mu.Lock()
	fn := r.cancelFn
	r.mu.Unlock()
	if fn != nil {
		fn()
	}
}

type result struct {
	vals []any
	err  error
}

func newRequest(class Class, fn Fn, ctx context.Context, ceiling, grace time.Duration) *request {
	return &request{
		class:   class,
		fn:      fn,
		caller:  ctx,
		reply:   make(chan result, 1),
		ceiling: ceiling,
		grace:   grace,
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
	stop    chan struct{} // closed by ForceReload to end loop
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

func (g *Generation) drain() { g.drainWith(ErrReloaded) }

// drainWith retires the generation and answers everything still queued
// with err. Hot mode only fails fast the Do calls that arrive after the
// wedge, so without this the requests already in the heap would wait on
// their reply channel forever.
func (g *Generation) drainWith(err error) {
	g.mu.Lock()
	g.retired = true
	pending := g.heap
	g.heap = nil
	g.mu.Unlock()
	for _, r := range pending {
		r.mu.Lock()
		if !r.finished {
			r.finished = true
			close(r.done)
			r.reply <- result{err: err}
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
			case <-g.stop:
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
	r.setCancel(cancel)
	ceiling := r.ceiling
	if ceiling <= 0 {
		ceiling = HardCeiling
	}
	grace := r.grace
	if grace <= 0 {
		grace = AbandonGrace
	}

	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		slow := time.AfterFunc(SoftBudget, func() {
			g.rt.log.Warn("lua", "handler exceeded soft budget", map[string]any{
				"class": r.class.String(), "budget": SoftBudget.String(),
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
				r.reply <- result{err: ErrAbandoned}
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

	// A frozen namespace that changed under the request means the surface
	// is no longer trustworthy: fail this request and wedge the runtime
	// rather than let later calls run against it (DESIGN §4, §5).
	if ierr := g.SB.CheckIntegrity(); ierr != nil {
		g.rt.goHot(ierr)
		if g.finish(r) {
			r.reply <- result{err: ierr}
		}
		cancel()
		return
	}

	elapsed := time.Since(started)
	if err == nil && elapsed > SoftBudget && reqCtx.Err() == nil {
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
		r.reply <- result{vals: vals, err: fmt.Errorf("%w: %v", ErrCanceled, reqCtx.Err())}
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
