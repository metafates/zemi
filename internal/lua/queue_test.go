package lua

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	glua "github.com/yuin/gopher-lua"
)

func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	r := newRuntime(t, RuntimeOptions{})
	if err := r.Boot(t.Context(), nil); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	return r
}

// newRuntime is a runtime the test stops, whatever boot steps it runs: a
// synctest bubble does not end while the dispatcher waits for requests.
func newRuntime(t *testing.T, opts RuntimeOptions) *Runtime {
	t.Helper()
	r := NewRuntime(opts)
	t.Cleanup(func() {
		if g := r.current(); g != nil {
			close(g.stop)
		}
	})
	return r
}

// TestRetiredGenerationFrames: a request the watchdog abandoned can keep
// running after a reload. Its pushes and pops must stay on its own
// generation, or live code reads the wrong origin (DESIGN §4).
func TestRetiredGenerationFrames(t *testing.T) {
	r := newTestRuntime(t)
	oldL := r.current().L
	stale := r.PushFrame(oldL, OriginProject, "old.lua")
	if err := r.ForceReload(t.Context(), nil, nil); err != nil {
		t.Fatalf("ForceReload: %v", err)
	}
	L := r.current().L
	live := r.PushFrame(L, OriginProject, "init.lua")

	stale()
	late := r.PushFrame(oldL, OriginUser, "late.lua")
	if got, path := r.CurrentOrigin(L), r.CurrentPath(L); got != OriginProject || path != "init.lua" {
		t.Fatalf("after stale push and pop: origin=%s path=%q, want project init.lua", got, path)
	}
	live()
	late()
	if got := r.CurrentOrigin(L); got != OriginHost {
		t.Fatalf("after live pop: origin=%s, want %s", got, OriginHost)
	}
}

// TestQueuePriorityOrder pins the queue's drain contract (DESIGN §4): while
// the interpreter is occupied, requests queue; on drain the queue pops ui
// first, then agent, then io, FIFO within a class.
func TestQueuePriorityOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newTestRuntime(t)

		var mu sync.Mutex
		var order []string
		record := func(label string) Fn {
			return func(L *glua.LState) ([]any, error) {
				mu.Lock()
				order = append(order, label)
				mu.Unlock()
				return nil, nil
			}
		}

		started := make(chan struct{})
		sleepDo := func(L *glua.LState) ([]any, error) {
			_, _ = record("io:sleep")(L)
			close(started)
			time.Sleep(150 * time.Millisecond)
			return nil, nil
		}
		var wg sync.WaitGroup
		wg.Go(func() { _, _ = r.Do(t.Context(), ClassIO, 0, 0, "test", sleepDo) })

		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("sleeper never started")
		}
		// Enqueued in reverse priority order while the interpreter is busy.
		wg.Go(func() { _, _ = r.Do(t.Context(), ClassAgent, 0, 0, "test", record("agent")) })
		wg.Go(func() { _, _ = r.Do(t.Context(), ClassIO, 0, 0, "test", record("io")) })
		wg.Go(func() { _, _ = r.Do(t.Context(), ClassUI, 0, 0, "test", record("ui")) })

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("requests did not drain")
		}

		mu.Lock()
		defer mu.Unlock()
		if want := []string{"io:sleep", "ui", "agent", "io"}; !slices.Equal(order, want) {
			t.Fatalf("execution order %v, want %v", order, want)
		}
	})
}

// TestQueueCancellationPreempts verifies §4 cancellation: the context is
// installed on the LState, the VM raises a catchable "context canceled" at
// the next opcode, and the caller gets context.Canceled without the runtime
// going hot. A request the ceiling stopped says so and is no Dropped one.
func TestQueueCancellationPreempts(t *testing.T) {
	for _, c := range []struct {
		name          string
		ceiling, stop time.Duration
		want          string
		dropped       bool
	}{
		{"caller cancels", 0, 30 * time.Millisecond, "context canceled", true},
		{"ceiling fires", 30 * time.Millisecond, 0, "stopped after its 30ms ceiling", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newTestRuntime(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if c.stop > 0 {
				time.AfterFunc(c.stop, cancel)
			}
			// a loop the stop did not preempt ends at the 1s class ceiling or,
			// past its own, abandoned: an error neither case wants
			_, err := r.Do(ctx, ClassUI, c.ceiling, 0, "test", func(L *glua.LState) ([]any, error) {
				return nil, L.DoString("while true do end")
			})
			if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), c.want) || Dropped(err) != c.dropped {
				t.Fatalf("err = %v (dropped %v), want context.Canceled saying %q (dropped %v)", err, Dropped(err), c.want, c.dropped)
			}
			if r.IsHot() {
				t.Fatal("runtime went hot from a plain cancellation")
			}
		})
	}
}

// TestCoroutineRunsUnderResumingRequest: a coroutine made in an earlier
// request resumes instead of failing as canceled, and one made at boot,
// with no context, is still preempted by the request resuming it.
func TestCoroutineRunsUnderResumingRequest(t *testing.T) {
	r := newRuntime(t, RuntimeOptions{})
	var spin glua.LValue
	boot := BootStep{Name: "spin", Run: func(_ context.Context, _ *Runtime, g *Generation) error {
		err := g.L.DoString(`return coroutine.create(function() while true do end end)`)
		spin = g.L.Get(-1)
		return err
	}}
	if err := r.Boot(t.Context(), []BootStep{boot}); err != nil {
		t.Fatal(err)
	}
	run := func(ceiling time.Duration, src string, arg glua.LValue) (glua.LValue, error) {
		var out glua.LValue
		_, err := r.Do(t.Context(), ClassIO, ceiling, 2*time.Second, "test", func(L *glua.LState) ([]any, error) {
			fn, err := L.LoadString(src)
			if err != nil {
				return nil, err
			}
			L.Push(fn)
			L.Push(arg)
			if err := L.PCall(1, 1, nil); err != nil {
				return nil, err
			}
			out = L.Get(-1)
			L.Pop(1)
			return nil, nil
		})
		return out, err
	}
	co, err := run(0, `return coroutine.wrap(function() coroutine.yield() end)`, glua.LNil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(0, `local w = ...; w()`, co); err != nil {
		t.Fatalf("resume in a later request: %v", err)
	}
	if _, err := run(100*time.Millisecond, `assert(coroutine.resume(...))`, spin); !errors.Is(err, context.Canceled) || r.IsHot() {
		t.Fatalf("boot coroutine spin: err %v, hot %v", err, r.IsHot())
	}
}

// TestQueueWatchdogAbandonHot verifies the §4 watchdog: a stuck request
// exceeding ceiling+grace is abandoned hard, the runtime goes hot (every
// Lua request fails fast), and ForceReload recovers.
func TestQueueWatchdogAbandonHot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		r := newRuntime(t, RuntimeOptions{Log: slog.New(slog.NewTextHandler(&logs, nil))})
		if err := r.Boot(t.Context(), nil); err != nil {
			t.Fatalf("Boot: %v", err)
		}
		release := make(chan struct{})
		defer close(release)

		start := time.Now()
		_, err := r.Do(t.Context(), ClassIO, 50*time.Millisecond, 50*time.Millisecond, "test",
			func(L *glua.LState) ([]any, error) {
				<-release
				return nil, nil
			})
		elapsed := time.Since(start)
		if !errors.Is(err, ErrAbandoned) {
			t.Fatalf("expected ErrAbandoned, got %v", err)
		}
		if elapsed < 90*time.Millisecond || elapsed > 500*time.Millisecond {
			t.Fatalf("abandon fired at %s, want ~100ms", elapsed)
		}
		if !r.IsHot() {
			t.Fatal("runtime not hot after abandon")
		}
		// the abandoned request names itself: the soft-budget warn carries the
		// handler, the hot cause the class and the name together
		logged := logs.String()
		if !strings.Contains(logged, `msg="handler exceeded soft budget"`) || !strings.Contains(logged, "class=io handler=test budget=10ms") {
			t.Fatalf("soft-budget warn does not name the handler: %s", logged)
		}
		if !strings.Contains(logged, "request in class io (test) exceeded ceiling 50ms + grace 50ms") {
			t.Fatalf("abandon cause does not name the request: %s", logged)
		}
		if _, err := r.Do(t.Context(), ClassUI, 0, 0, "test",
			func(L *glua.LState) ([]any, error) { return nil, nil }); !errors.Is(err, ErrHot) {
			t.Fatalf("expected ErrHot while hot, got %v", err)
		}

		if err := r.ForceReload(t.Context(), nil, nil); err != nil {
			t.Fatalf("ForceReload: %v", err)
		}
		if r.IsHot() {
			t.Fatal("runtime still hot after ForceReload")
		}
		vals, err := r.Do(t.Context(), ClassUI, 0, 0, "test",
			func(L *glua.LState) ([]any, error) {
				return []any{L.GetGlobal("bootmark") == glua.LNil}, nil
			})
		if err != nil {
			t.Fatalf("Do after reload: %v", err)
		}
		if fresh, _ := vals[0].(bool); !fresh {
			t.Fatal("reloaded generation is not fresh")
		}
	})
}

// TestRetiredWatchdogLeavesFreshRuntime: a request wedged across a reload
// is abandoned by its old generation's watchdog, which must not put the
// fresh generation hot.
func TestRetiredWatchdogLeavesFreshRuntime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newTestRuntime(t)
		started, release := make(chan struct{}), make(chan struct{})
		defer close(release)
		errc := make(chan error, 1)
		go func() {
			_, err := r.Do(t.Context(), ClassIO, 50*time.Millisecond, 50*time.Millisecond, "test",
				func(L *glua.LState) ([]any, error) {
					close(started)
					<-release
					return nil, nil
				})
			errc <- err
		}()
		<-started
		if err := r.ForceReload(t.Context(), nil, nil); err != nil {
			t.Fatalf("ForceReload: %v", err)
		}
		if err := <-errc; !errors.Is(err, ErrAbandoned) {
			t.Fatalf("wedged request: %v, want ErrAbandoned", err)
		}
		if r.IsHot() {
			t.Fatal("retired generation's watchdog put the fresh runtime hot")
		}
	})
}

// TestCloseWaitsForTheRetiredDispatcher: the purge that follows a retire
// must not race a request still running on the old LState, so retire waits
// for the dispatcher to leave it (DESIGN §4).
func TestCloseWaitsForTheRetiredDispatcher(t *testing.T) {
	r := newTestRuntime(t)
	started, finished := make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = r.Do(t.Context(), ClassIO, 0, 0, "test", func(*glua.LState) ([]any, error) {
			close(started)
			time.Sleep(100 * time.Millisecond)
			close(finished)
			return nil, nil
		})
	}()
	<-started
	r.Close()
	select {
	case <-finished:
	default:
		t.Fatal("Close returned while the retired dispatcher was still running")
	}
}

// TestQueuePendingDroppedOnReload verifies that requests queued behind a
// busy interpreter are answered with ErrReloaded when ForceReload retires
// the generation (DESIGN §4: reload purges every registration of the dead
// generation before replay), and the running one is canceled: left going,
// it would see the fresh generation's origin frames.
func TestQueuePendingDroppedOnReload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newTestRuntime(t)

		started := make(chan struct{})
		running := make(chan error, 1)
		go func() {
			_, err := r.Do(t.Context(), ClassIO, 10*time.Second, time.Second, "test",
				func(L *glua.LState) ([]any, error) {
					close(started)
					<-L.Context().Done()
					return nil, L.Context().Err()
				})
			running <- err
		}()
		<-started

		pending := make(chan error, 1)
		go func() {
			_, err := r.Do(t.Context(), ClassUI, 0, 0, "test",
				func(L *glua.LState) ([]any, error) { return nil, nil })
			pending <- err
		}()
		synctest.Wait() // it waits in the queue

		if err := r.ForceReload(t.Context(), nil, nil); err != nil {
			t.Fatalf("ForceReload: %v", err)
		}
		if err := <-pending; !errors.Is(err, ErrReloaded) {
			t.Fatalf("pending request got %v, want ErrReloaded", err)
		}
		if err := <-running; !errors.Is(err, context.Canceled) {
			t.Fatalf("running request got %v, want canceled", err)
		}
	})
}

// TestChunkAfterReload: a request still running when ForceReload retires its
// generation compiles nothing against the fresh sandbox or a nil one.
func TestChunkAfterReload(t *testing.T) {
	r := newTestRuntime(t)
	old := r.current().L
	if err := r.ForceReload(t.Context(), nil, nil); err != nil {
		t.Fatalf("ForceReload: %v", err)
	}
	if err := r.runChunk(t.Context(), old, "return 1", "late.lua", OriginUser); !errors.Is(err, ErrReloaded) {
		t.Fatalf("late chunk got %v, want ErrReloaded", err)
	}
}

// TestQueueForceReloadReplays verifies that a reload boots a fresh
// generation and replays the boot steps on it.
func TestQueueForceReloadReplays(t *testing.T) {
	r := newRuntime(t, RuntimeOptions{})

	var boots int
	var purgeCount int
	steps := []BootStep{{
		Name: "mark",
		Run: func(ctx context.Context, r *Runtime, g *Generation) error {
			boots++
			g.L.SetGlobal("genmark", glua.LNumber(g.ID))
			return nil
		},
	}}

	if err := r.Boot(t.Context(), steps); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	readMark := func() int64 {
		vals, err := r.Do(t.Context(), ClassUI, 0, 0, "test",
			func(L *glua.LState) ([]any, error) {
				n, _ := L.GetGlobal("genmark").(glua.LNumber)
				return []any{int64(n)}, nil
			})
		if err != nil {
			t.Fatalf("read genmark: %v", err)
		}
		return vals[0].(int64)
	}
	if boots != 1 {
		t.Fatalf("boot steps ran %d times, want 1", boots)
	}
	first := readMark()

	if err := r.ForceReload(t.Context(), steps, func() { purgeCount++ }); err != nil {
		t.Fatalf("ForceReload: %v", err)
	}
	if boots != 2 {
		t.Fatalf("boot steps ran %d times after reload, want 2", boots)
	}
	if purgeCount != 1 {
		t.Fatalf("purge ran %d times, want 1", purgeCount)
	}
	if second := readMark(); first == 0 || second == 0 || first == second {
		t.Fatalf("generation id not replaced on reload: %d -> %d", first, second)
	}
}

// TestQueueHotAnswersQueued: when the watchdog wedges the interpreter,
// requests already in the heap are answered with ErrHot. Hot mode only
// fails fast the Do calls that arrive later, so without the drain these
// would wait on their reply channel forever.
func TestQueueHotAnswersQueued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newTestRuntime(t)

		started, release := make(chan struct{}), make(chan struct{})
		defer close(release)
		go func() {
			_, _ = r.Do(t.Context(), ClassIO, 50*time.Millisecond, 50*time.Millisecond, "test",
				func(L *glua.LState) ([]any, error) {
					close(started)
					<-release
					return nil, nil
				})
		}()
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("wedger never started")
		}

		queued := make(chan error, 1)
		go func() {
			_, err := r.Do(t.Context(), ClassUI, 0, 0, "test",
				func(L *glua.LState) ([]any, error) { return nil, nil })
			queued <- err
		}()
		time.Sleep(20 * time.Millisecond) // reach the heap before the abandon

		select {
		case err := <-queued:
			if !errors.Is(err, ErrHot) {
				t.Fatalf("queued request got %v, want ErrHot", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("queued request never answered after the wedge")
		}
	})
}

// TestQueueIntegrityGoesHot: a frozen namespace mutated behind the
// sandbox fails its request and wedges the runtime, so a tampered surface
// never serves later calls.
func TestQueueIntegrityGoesHot(t *testing.T) {
	r := newRuntime(t, RuntimeOptions{})
	var ns *glua.LTable
	step := BootStep{Name: "ns", Run: func(ctx context.Context, rt *Runtime, g *Generation) error {
		ns = g.L.NewTable()
		ns.RawSetString("fn", g.L.NewFunction(func(*glua.LState) int { return 0 }))
		return g.SB.RegisterNamespace("zemi.ui", ns, nil)
	}}
	if err := r.Boot(t.Context(), []BootStep{step}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	noop := func(L *glua.LState) ([]any, error) { return nil, nil }
	if _, err := r.Do(t.Context(), ClassUI, 0, 0, "test", noop); err != nil {
		t.Fatalf("clean request: %v", err)
	}
	_, err := r.Do(t.Context(), ClassUI, 0, 0, "test", func(L *glua.LState) ([]any, error) {
		ns.RawSetString("sneaked", glua.LTrue) // a Go-side bypass of the freeze
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("tampered namespace returned %v, want an integrity error", err)
	}
	if !r.IsHot() {
		t.Fatal("runtime not hot after an integrity failure")
	}
}

// TestEval: the lua tool runs through Eval in a wrapper env, which must keep
// the chunk env's __newindex guard so assigning a protected global raises
// instead of silently shadowing it, and a result that cannot be marshaled
// is rendered rather than failing a chunk that already ran.
func TestEval(t *testing.T) {
	r := newRuntime(t, RuntimeOptions{})
	step := BootStep{Name: "ns", Run: func(ctx context.Context, rt *Runtime, g *Generation) error {
		tbl := g.L.NewTable()
		tbl.RawSetString("fn", g.L.NewFunction(func(*glua.LState) int { return 0 }))
		return g.SB.RegisterNamespace("zemi.ui", tbl, nil)
	}}
	if err := r.Boot(t.Context(), []BootStep{step}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	discard := func(string) {}
	if _, err := r.Eval(t.Context(), `zemi = { ui = false }`, "lua-tool", OriginProject, time.Second, discard); err == nil {
		t.Fatal("assignment to the protected zemi global was accepted")
	}
	vals, err := r.Eval(t.Context(), `x = 5 return x`, "lua-tool", OriginProject, time.Second, discard)
	if err != nil || len(vals) != 1 {
		t.Fatalf("ordinary global in the wrapper env: %v (%v)", err, vals)
	}
	cyclic := `local t = {} t.t = t return setmetatable(t, {__tostring = function() error("boom") end})`
	if vals, err := r.Eval(t.Context(), cyclic, "lua-tool", OriginProject, time.Second, discard); err != nil || len(vals) != 1 {
		t.Fatalf("unmarshalable result with a raising __tostring: %v (%v)", err, vals)
	}
	// a one-chunk error is its message; a nested call keeps the frame that
	// names the function
	for _, c := range []struct{ src, want, not string }{
		{"error('boom')", "lua-tool:1: boom", "traceback"},
		{"local function f() error('boom') end\nf()", "in function 'f'", "@user:"},
		{"x = = 1", "lua-tool:1: ", "@user:"},
	} {
		_, err := r.Eval(t.Context(), c.src, "lua-tool", OriginUser, time.Second, discard)
		if err == nil || !strings.Contains(err.Error(), c.want) || c.not != "" && strings.Contains(err.Error(), c.not) {
			t.Errorf("%q: %v", c.src, err)
		}
	}
}

// TestRaiseEscapingRequestUnwinds: a Lua error that escapes a request
// through an unprotected Call leaves no frames or values behind for the
// next request.
func TestRaiseEscapingRequestUnwinds(t *testing.T) {
	r := newTestRuntime(t)
	raise := func(L *glua.LState) ([]any, error) {
		fn, _ := L.LoadString(`error("escaped")`)
		L.Push(fn)
		L.Call(0, 0)
		return nil, nil
	}
	if _, err := r.Do(t.Context(), ClassIO, 0, 0, "test", raise); err == nil || !strings.Contains(err.Error(), "escaped") {
		t.Fatalf("escaping raise: %v", err)
	}
	_, _ = r.Do(t.Context(), ClassIO, 0, 0, "test", func(L *glua.LState) ([]any, error) {
		if _, deeper := L.GetStack(1); deeper || L.GetTop() != 0 {
			t.Errorf("next request starts on a dirty state: frames below %v, top %d", deeper, L.GetTop())
		}
		return nil, nil
	})
}

// TestQueueDoBeforeBoot fails fast instead of panicking.
func TestQueueDoBeforeBoot(t *testing.T) {
	r := newRuntime(t, RuntimeOptions{})
	_, err := r.Do(t.Context(), ClassUI, 0, 0, "test",
		func(L *glua.LState) ([]any, error) { return nil, nil })
	if err == nil || err.Error() != "zemi: lua runtime not booted" {
		t.Fatalf("expected not-booted error, got %v", err)
	}
}

// TestReloadWaitsForBoot: a reload asked for while the first boot runs
// (a slow plugin, the trust prompt) waits for it, so two boots never push
// origin frames onto the one shared stack at once.
func TestReloadWaitsForBoot(t *testing.T) {
	r := newRuntime(t, RuntimeOptions{})
	started, release := make(chan struct{}), make(chan struct{})
	booted := make(chan error, 1)
	slow := BootStep{Name: "slow", Run: func(context.Context, *Runtime, *Generation) error {
		close(started)
		<-release
		return nil
	}}
	go func() { booted <- r.Boot(t.Context(), []BootStep{slow}) }()
	<-started
	reloaded := make(chan error, 1)
	go func() { reloaded <- r.ForceReload(t.Context(), nil, nil) }()
	select {
	case <-reloaded:
		t.Fatal("reload ran while the first boot was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-booted; err != nil {
		t.Fatal(err)
	}
	if err := <-reloaded; err != nil {
		t.Fatal(err)
	}
}

// TestQueueDoAfterFailedBoot: a boot step that fails leaves a generation
// with no dispatcher, so requests fail fast until a reload succeeds.
func TestQueueDoAfterFailedBoot(t *testing.T) {
	r := newRuntime(t, RuntimeOptions{})
	fail := BootStep{Name: "fail", Run: func(context.Context, *Runtime, *Generation) error { return errors.New("bad config") }}
	if err := r.Boot(t.Context(), []BootStep{fail}); err == nil {
		t.Fatal("boot with a failing step succeeded")
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.Do(t.Context(), ClassUI, 0, 0, "test", func(*glua.LState) ([]any, error) { return nil, nil })
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrHot) {
			t.Fatalf("request after a failed boot: %v, want ErrHot", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request after a failed boot never answered")
	}
	if err := r.ForceReload(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Do(t.Context(), ClassUI, 0, 0, "test", func(*glua.LState) ([]any, error) { return nil, nil }); err != nil {
		t.Fatalf("request after a good reload: %v", err)
	}
}

// TestReloadWindowReportsReloaded: a request landing between generations
// (the purge stops MCP and language servers, so the window is real) reads
// as a reload, not as a runtime nobody booted, which the UI and the host
// would record as an error.
func TestReloadWindowReportsReloaded(t *testing.T) {
	noop := func(*glua.LState) ([]any, error) { return nil, nil }
	var during error
	r := newTestRuntime(t)
	purge := func() {
		_, during = r.Do(t.Context(), ClassUI, 0, 0, "test", noop)
	}
	if err := r.ForceReload(t.Context(), nil, purge); err != nil {
		t.Fatalf("ForceReload: %v", err)
	}
	if !errors.Is(during, ErrReloaded) {
		t.Fatalf("during the purge: %v, want ErrReloaded", during)
	}
	if _, err := newRuntime(t, RuntimeOptions{}).Do(t.Context(), ClassUI, 0, 0, "test", noop); errors.Is(err, ErrReloaded) {
		t.Fatalf("a runtime that never booted reports a reload: %v", err)
	}
}
