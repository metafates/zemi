package lua

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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

// Dropped reports whether err is the runtime dropping a request (hot,
// reloading, cancelled) rather than the Lua code failing.
func Dropped(err error) bool {
	return errors.Is(err, ErrHot) || errors.Is(err, ErrReloaded) || errors.Is(err, ErrCanceled)
}

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

// cancel preempts the request if it is already running. A request still
// waiting in the heap has no cancel func yet: it runs when popped, with its
// caller's cancelled context, or drain answers it.
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

// Generation is one interpreter instance: an LState, its queue and its
// dispatcher goroutine. A wedged generation is dropped wholesale; the
// replacement boots fresh (DESIGN §4 hot mode).
type Generation struct {
	ID      int64
	rt      *Runtime
	SB      *Sandbox
	L       *glua.LState
	modules map[string]glua.LValue
	// Plugins are the symlink-resolved dirs of the plugins the plugins boot
	// step loaded: the only plugin code require resolves this generation.
	Plugins []string

	mu      sync.Mutex
	heap    []*request
	running *request
	seq     uint64
	signal  chan struct{}
	stop    chan struct{} // closed by ForceReload to end loop
	retired bool

	frames []frame // origin stack, see pushFrame

	// tasks maps a task's thread to it until the task ends (Suspend reads
	// it, coroutine.resume refuses it); ctx ends with the generation, which
	// ends every task's wait.
	tasks  map[*glua.LState]*taskRun
	ctx    context.Context
	cancel context.CancelCauseFunc
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

// drain retires the generation, answers everything still queued with
// err and cancels the running request. Hot mode only fails fast the Do
// calls that arrive after the wedge, so without this the requests already
// in the heap would wait on their reply channel forever. A running request
// left going would push and read origin frames of the next generation.
func (g *Generation) drain(err error) {
	g.cancel(err)
	g.mu.Lock()
	g.retired = true
	pending, running := g.heap, g.running
	g.heap = nil
	g.mu.Unlock()
	if running != nil {
		running.cancel()
	}
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
	reqCtx, cancel := context.WithCancel(r.caller)
	defer cancel()
	r.setCancel(cancel)
	g.mu.Lock()
	retired := g.retired
	if !retired {
		g.running = r
	}
	g.mu.Unlock()
	if retired {
		r.reply <- result{err: ErrReloaded}
		return
	}
	defer func() {
		g.mu.Lock()
		g.running = nil
		g.mu.Unlock()
	}()
	ceiling := r.ceiling
	if ceiling <= 0 {
		ceiling = HardCeiling
	}
	grace := r.grace
	if grace <= 0 {
		grace = AbandonGrace
	}

	var fired atomic.Bool
	go func() {
		slow := time.AfterFunc(SoftBudget, func() {
			g.rt.log.Warn("lua", "handler exceeded soft budget", map[string]any{
				"class": r.class.String(), "budget": SoftBudget.String(),
			})
		})
		defer slow.Stop()
		hard := time.AfterFunc(ceiling, func() { fired.Store(true); cancel() })
		defer hard.Stop()
		select {
		case <-r.done:
		case <-time.After(ceiling + grace):
			if g.finish(r) {
				cancel()
				// Mark hot BEFORE answering: while hot every Lua request
				// fails fast, so a request arriving after the abandon
				// cannot enqueue onto the wedged interpreter.
				g.rt.goHot(g, fmt.Errorf("request in class %s exceeded ceiling %s + grace %s",
					r.class.String(), ceiling, grace))
				r.reply <- result{err: ErrAbandoned}
			}
		}
	}()
	defer close(r.done)

	started := time.Now()
	g.L.SetContext(reqCtx)
	pop := g.pushFrame(OriginHost, "")
	vals, err := g.safeRun(r)
	pop()
	g.L.RemoveContext()

	// A frozen namespace that changed under the request means the surface
	// is no longer trustworthy: fail this request and wedge the runtime
	// rather than let later calls run against it (DESIGN §4, §5).
	if ierr := g.SB.CheckIntegrity(); ierr != nil {
		g.rt.goHot(g, ierr)
		if g.finish(r) {
			r.reply <- result{err: ierr}
		}
		return
	}

	elapsed := time.Since(started)
	if err == nil && elapsed > SoftBudget && reqCtx.Err() == nil {
		g.rt.log.Debug("lua", "slow handler completed", map[string]any{
			"class": r.class.String(), "elapsed": elapsed.String(),
		})
	}

	// The watchdog may abandon the request up to this point; whoever
	// finishes it first owns the single reply the buffered channel holds.
	if !g.finish(r) {
		return
	}
	switch {
	case reqCtx.Err() != nil:
		// A reply arriving after cancellation is canceled, whatever the Lua
		// returned (DESIGN §4).
		err := reqCtx.Err()
		if fired.Load() && r.caller.Err() == nil {
			err = ceilingError(ceiling)
		}
		r.reply <- result{err: err}
	case err != nil:
		r.reply <- result{err: err}
	default:
		r.reply <- result{vals: vals}
	}
}

// ceilingError is a request the hard ceiling stopped: it says so, and is
// still context.Canceled to errors.Is.
type ceilingError time.Duration

func (e ceilingError) Error() string {
	return fmt.Sprintf("stopped after its %s ceiling: move slow work to zemi.job or zemi.timer", time.Duration(e))
}

func (ceilingError) Unwrap() error { return context.Canceled }

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

// safeRun runs fn inside PCall: a raise or Go panic escaping fn (an
// unprotected Call, a bad FromGo) then unwinds the LState's call and value
// stacks instead of leaving its frames there for every later request.
func (g *Generation) safeRun(r *request) (vals []any, err error) {
	g.L.Push(g.L.NewFunction(func(L *glua.LState) int {
		vals, err = r.fn(L)
		return 0
	}))
	if perr := g.L.PCall(0, 0, nil); perr != nil {
		err = fmt.Errorf("zemi bug: lua host function panicked: %w", perr)
		g.rt.log.Error("lua", err.Error(), nil)
	}
	return vals, err
}
