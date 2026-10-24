package lua

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	glua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"
)

// Task is a Lua callback run as a coroutine (DESIGN §4): a wait() in it
// suspends the task and ends the request running it, so the interpreter
// serves other requests until what it waits on is done. Each slice is an
// ordinary request of Class, so the watchdog, the priorities and ctx
// cancellation hold per slice.
type Task struct {
	Class   Class
	Ceiling time.Duration // per slice; 0 is the class default
	Origin  Origin
	Path    string
	// Start runs on the interpreter before the first slice and returns the
	// function and its arguments; a nil function or an error ends the task.
	Start func(L *glua.LState) (*glua.LFunction, []glua.LValue, error)
	// Done, when set, reads what the function returned, on the interpreter.
	Done func(L *glua.LState, rets []glua.LValue) ([]any, error)
}

// Waiter blocks off the interpreter until what a task waits on is done or
// ctx ends, and returns the values wait() gives the task when it resumes.
type Waiter func(ctx context.Context) func(L *glua.LState) []glua.LValue

// NOTE: every task gets a fresh thread (a registry and a call stack, some
// 100 KB of garbage); a per-generation pool is the upgrade path, which
// gopher-lua makes awkward by killing a thread that returns.
type taskRun struct {
	Task
	gen  *Generation
	th   *glua.LState
	wait Waiter                             // set by Suspend before its yield
	push func(L *glua.LState) []glua.LValue // what the waiter handed back
}

// RunTask runs t and waits for it to end, suspensions included.
func (r *Runtime) RunTask(ctx context.Context, t Task) ([]any, error) {
	tr := &taskRun{Task: t}
	vals, err := r.slice(ctx, tr)
	for err == nil && tr.wait != nil {
		vals, err = r.next(ctx, tr)
	}
	return vals, err
}

// StartTask runs t's first slice and returns its error. A task that
// suspends goes on off the caller's goroutine, and end gets how it ended,
// so the caller's next request runs before the rest of this one; with
// WholeTasks it is RunTask.
func (r *Runtime) StartTask(ctx context.Context, t Task, end func(error)) error {
	if r.wholeTasks {
		_, err := r.RunTask(ctx, t)
		return err
	}
	tr := &taskRun{Task: t}
	if _, err := r.slice(ctx, tr); err != nil || tr.wait == nil {
		return err
	}
	go func() {
		var err error
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("zemi bug: task panicked: %v", p)
				r.log.Error(err.Error(), "origin", "lua")
			}
			end(err)
		}()
		for err == nil && tr.wait != nil {
			_, err = r.next(ctx, tr)
		}
	}()
	return nil
}

// next waits for what the task waits on, then runs its next slice. A purged
// generation cancels the wait, so a dropped task ends at once.
func (r *Runtime) next(ctx context.Context, tr *taskRun) ([]any, error) {
	wctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(tr.gen.ctx, cancel)
	tr.push = tr.wait(wctx)
	stop()
	cancel()
	tr.wait = nil
	if err := ctx.Err(); err != nil {
		// only the actor may touch the thread the task leaves suspended
		tr.gen.enqueue(newRequest(context.Background(), ClassIO, func(*glua.LState) ([]any, error) {
			tr.gen.endTask(tr.th)
			return nil, nil
		}, 0, 0))
		return nil, err
	}
	if tr.gen.ctx.Err() != nil {
		return nil, context.Cause(tr.gen.ctx)
	}
	return r.slice(ctx, tr)
}

// slice runs one request of the task: the first on the current generation,
// the rest on the one it started on.
func (r *Runtime) slice(ctx context.Context, tr *taskRun) ([]any, error) {
	fn := func(L *glua.LState) ([]any, error) { return tr.run(L) }
	if tr.th == nil {
		return r.Do(ctx, tr.Class, tr.Ceiling, 0, fn)
	}
	if r.hot.Load() {
		return nil, ErrHot
	}
	return tr.gen.do(ctx, tr.Class, tr.Ceiling, 0, fn)
}

func (tr *taskRun) run(L *glua.LState) ([]any, error) {
	g := genOf(L)
	var fn *glua.LFunction
	var args []glua.LValue
	if tr.th == nil {
		f, a, err := tr.Start(L)
		if f == nil || err != nil {
			return nil, err
		}
		fn, args = f, a
		tr.gen = g
		tr.th, _ = L.NewThread()
	} else {
		args = tr.push(L)
		tr.push = nil
	}
	if fn != nil && fn.IsG {
		fn = Suspending(L, fn) // a thread cannot start on a Go function
	}
	defer g.pushFrame(tr.Origin, tr.Path)()
	bindContext(L, tr.th)
	g.tasks[tr.th] = tr
	st, rets, err := Resume(L, tr.th, fn, args...)
	if st != glua.ResumeYield || tr.wait == nil {
		g.endTask(tr.th)
	}
	if ae, ok := errors.AsType[*glua.ApiError](err); ok && ae.StackTrace == "" {
		ae.StackTrace = traceback(tr.th) // the one PCall would give
	}
	switch {
	case err != nil:
		tr.wait = nil
		return nil, err
	case st == glua.ResumeYield && tr.wait == nil:
		return nil, errors.New("coroutine.yield cannot suspend a task: wait on a handle instead")
	case st == glua.ResumeYield:
		return nil, nil
	case tr.wait != nil:
		// the yield unwound a metamethod call instead of the thread
		tr.wait = nil
		return nil, errors.New("wait ran where it cannot suspend, such as inside a metamethod")
	case tr.Done == nil:
		return nil, nil
	}
	return tr.Done(L, rets)
}

// endTask forgets a task's thread and kills it, so Lua holding the thread
// cannot resume what the task left suspended.
func (g *Generation) endTask(th *glua.LState) {
	delete(g.tasks, th)
	th.Dead = true
}

// Suspend yields the task running on L until w returns (DESIGN §4). Call
// it from a function wrapped by Suspending.
func Suspend(L *glua.LState, api string, w Waiter) int {
	CheckWait(L, api)
	genOf(L).tasks[L].wait = w
	return L.Yield()
}

// CheckWait raises unless L is a task that may suspend here: not outside a
// task, not in a coroutine the task made, and not under a Go call such as
// pcall, since the yield would unwind that call instead.
func CheckWait(L *glua.LState, api string) {
	var msg string
	switch {
	case genOf(L).tasks[L] == nil:
		msg = api + ": only a task can wait: a Lua tool's execute, a command, a keymap, an overlay callback, a tool.post handler, a lua tool script, or an on_done or on_exit callback (pass on_done or on_exit here instead)"
	case InGoCall(L):
		msg = api + ": cannot wait inside pcall or a library callback such as table.sort; call it outside them, where a failure comes back as nil, err"
	default:
		return
	}
	// level 3: the caller of the Suspending wrapper, which is level 2
	L.Error(glua.LString(msg), 3)
}

// InGoCall reports a Go function between the one running on L and the
// bottom of L's stack (pcall, table.sort, zemi.command.run): a yield there
// unwinds that call instead of suspending the thread.
func InGoCall(L *glua.LState) bool {
	for level := 1; ; level++ {
		dbg, ok := L.GetStack(level)
		if !ok {
			return false
		}
		if f, err := L.GetInfo("f", dbg, glua.LNil); err == nil {
			if fn, ok := f.(*glua.LFunction); ok && fn.IsG {
				return true
			}
		}
	}
}

// Resume resumes th from L. A raise comes back without a traceback, and a
// VM panic (a yield that unwound a Go library callback) as an error, with
// L's stack and current thread put back so L stays usable.
func Resume(L, th *glua.LState, fn *glua.LFunction, args ...glua.LValue) (st glua.ResumeState, vals []glua.LValue, err error) {
	if fn == nil {
		// a Suspending wrapper reads three results off the registers the
		// resume values land in; fewer would leave a stale one there
		for len(args) < 3 {
			args = append(args, glua.LNil)
		}
	}
	top := L.GetTop()
	defer func() {
		if p := recover(); p != nil {
			L.SetTop(top)
			L.G.CurrentThread = L
			st, vals, err = glua.ResumeError, nil, fmt.Errorf("a wait ran where it cannot suspend (inside pcall or a library callback such as table.sort): %v", p)
		}
	}()
	st, err, vals = L.Resume(th, fn, args...)
	return st, vals, err
}

// traceback renders a dead thread's frames in gopher-lua's shape. The
// frame that raised is already popped; its line is in the message.
func traceback(th *glua.LState) string {
	lines := []string{"stack traceback:"}
	for level := range 20 {
		dbg, ok := th.GetStack(level)
		if !ok {
			break
		}
		if _, err := th.GetInfo("nSl", dbg, glua.LNil); err != nil {
			continue
		}
		var line string
		switch {
		case dbg.What == "G":
			line = "\t[G]: in function '" + dbg.Name + "'"
		case dbg.Name != "" && dbg.What != "main":
			line = fmt.Sprintf("\t%s:%d: in function '%s'", dbg.Source, dbg.CurrentLine, dbg.Name)
		default:
			// the task's own function; a chunk's ends in ":0>", which chunkErr drops
			line = fmt.Sprintf("\t%s:%d: in function <%s:%d>", dbg.Source, dbg.CurrentLine, dbg.Source, dbg.LineDefined)
		}
		if line == lines[len(lines)-1] {
			break // GetStack repeats the bottom frame past a tail call
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

var suspendingProto = func() *glua.FunctionProto {
	chunk, err := parse.Parse(strings.NewReader(`local f = ... return function(...) local a, b, e = f(...) if e ~= nil then error(e, 0) end return a, b end`), "wait")
	if err != nil {
		panic(err)
	}
	// host, so effective() reads through it like the Go function it wraps
	proto, err := glua.Compile(chunk, OriginHost.ChunkName("wait"))
	if err != nil {
		panic(err)
	}
	return proto
}()

// Suspending wraps fn, a Go function that may call Suspend, in a Lua
// function: gopher-lua drops the resume values of a Go function reached by
// a tail call (`return h:wait()`), and the wrapper calls it in non-tail
// position. A third value the waiter hands back is raised at the call, as
// is. GuardFuncs guards the fn inside.
func Suspending(L *glua.LState, fn *glua.LFunction) *glua.LFunction {
	L.Push(L.NewFunctionFromProto(suspendingProto))
	L.Push(fn)
	L.Call(1, 1)
	w := L.Get(-1).(*glua.LFunction)
	L.Pop(1)
	return w
}

// suspended is fn's inner Go function when fn is a Suspending wrapper.
func suspended(fn *glua.LFunction) (*glua.LFunction, bool) {
	if fn.IsG || fn.Proto != suspendingProto.FunctionPrototypes[0] {
		return nil, false
	}
	inner, ok := fn.Upvalues[0].Value().(*glua.LFunction)
	return inner, ok && inner.IsG
}

// RaiseScript raises format attributed to the innermost script frame, so an
// error a host call raises reads as the script's line and never as the
// Suspending wrapper's ("@host:wait:1"). Host machinery - Go frames, the
// wrapper, unnamed chunks - is skipped; with no script frame (a tail call
// replaced it) the message raises bare, since a wrapper position would only
// misattribute it.
func RaiseScript(L *glua.LState, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	for level := 0; ; level++ {
		dbg, ok := L.GetStack(level)
		if !ok {
			L.Error(glua.LString(msg), 0)
			return
		}
		fv, err := L.GetInfo("f", dbg, glua.LNil)
		if err != nil {
			continue
		}
		fn, ok := fv.(*glua.LFunction)
		if !ok {
			continue
		}
		switch {
		case fn.IsG, fn.Proto == suspendingProto.FunctionPrototypes[0]:
		case fn.Proto.SourceName == "" || fn.Proto.SourceName == "[G]" || fn.Proto.SourceName == "<string>":
		default:
			L.Error(glua.LString(msg), level+1)
			return
		}
	}
}
