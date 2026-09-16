package lua

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	glua "github.com/yuin/gopher-lua"
)

func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	r := NewRuntime(RuntimeOptions{})
	if err := r.Boot(context.Background(), nil); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	return r
}

// TestLateFramePopIgnored: a request the watchdog abandoned can return
// after a reload. Its pop must not take a frame of the live generation,
// which would misattribute the running chunk's origin (DESIGN §4).
func TestLateFramePopIgnored(t *testing.T) {
	r := newTestRuntime(t)
	stale := r.PushFrame(OriginProject, "old.lua")
	if err := r.ForceReload(context.Background(), nil, nil); err != nil {
		t.Fatalf("ForceReload: %v", err)
	}
	live := r.PushFrame(OriginUser, "init.lua")

	stale()
	if got, path := r.CurrentOrigin(), r.CurrentPath(); got != OriginUser || path != "init.lua" {
		t.Fatalf("after late pop: origin=%s path=%q, want user init.lua", got, path)
	}
	live()
	if got := r.CurrentOrigin(); got != OriginHost {
		t.Fatalf("after live pop: origin=%s, want %s", got, OriginHost)
	}
}

// TestQueuePriorityOrder pins the queue's drain contract (DESIGN §4): while
// the interpreter is occupied, requests queue; on drain the queue pops ui
// first, then agent, then io, FIFO within a class.
func TestQueuePriorityOrder(t *testing.T) {
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
	wg.Add(4)
	go func() { defer wg.Done(); _, _ = r.Do(context.Background(), ClassIO, 0, 0, sleepDo) }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("sleeper never started")
	}
	// Enqueued in reverse priority order while the interpreter is busy.
	go func() { defer wg.Done(); _, _ = r.Do(context.Background(), ClassAgent, 0, 0, record("agent")) }()
	go func() { defer wg.Done(); _, _ = r.Do(context.Background(), ClassIO, 0, 0, record("io")) }()
	go func() { defer wg.Done(); _, _ = r.Do(context.Background(), ClassUI, 0, 0, record("ui")) }()

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
}

// TestQueueCancellationPreempts verifies §4 cancellation: the context is
// installed on the LState, the VM raises a catchable "context canceled" at
// the next opcode, and the caller gets context.Canceled without the runtime
// going hot.
func TestQueueCancellationPreempts(t *testing.T) {
	r := newTestRuntime(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := r.ExecuteChunk(ctx, "while true do end", "spin.lua", OriginUser)
	elapsed := time.Since(start)
	cancel()

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed > 900*time.Millisecond {
		t.Fatalf("cancellation did not preempt the loop: %s", elapsed)
	}
	if r.IsHot() {
		t.Fatal("runtime went hot from a plain cancellation")
	}
}

// TestCoroutineRunsUnderResumingRequest: a coroutine made in an earlier
// request resumes instead of failing as canceled, and one made at boot,
// with no context, is still preempted by the request resuming it.
func TestCoroutineRunsUnderResumingRequest(t *testing.T) {
	r := NewRuntime(RuntimeOptions{})
	var spin glua.LValue
	boot := BootStep{Name: "spin", Run: func(_ context.Context, _ *Runtime, g *Generation) error {
		err := g.L.DoString(`return coroutine.create(function() while true do end end)`)
		spin = g.L.Get(-1)
		return err
	}}
	if err := r.Boot(context.Background(), []BootStep{boot}); err != nil {
		t.Fatal(err)
	}
	run := func(ceiling time.Duration, src string, arg glua.LValue) (glua.LValue, error) {
		var out glua.LValue
		_, err := r.Do(context.Background(), ClassIO, ceiling, 2*time.Second, func(L *glua.LState) ([]any, error) {
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
	r := newTestRuntime(t)

	start := time.Now()
	_, err := r.Do(context.Background(), ClassIO, 50*time.Millisecond, 50*time.Millisecond,
		func(L *glua.LState) ([]any, error) {
			time.Sleep(750 * time.Millisecond)
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
	if _, err := r.Do(context.Background(), ClassUI, 0, 0,
		func(L *glua.LState) ([]any, error) { return nil, nil }); !errors.Is(err, ErrHot) {
		t.Fatalf("expected ErrHot while hot, got %v", err)
	}

	if err := r.ForceReload(context.Background(), nil, nil); err != nil {
		t.Fatalf("ForceReload: %v", err)
	}
	if r.IsHot() {
		t.Fatal("runtime still hot after ForceReload")
	}
	vals, err := r.Do(context.Background(), ClassUI, 0, 0,
		func(L *glua.LState) ([]any, error) {
			return []any{L.GetGlobal("bootmark") == glua.LNil}, nil
		})
	if err != nil {
		t.Fatalf("Do after reload: %v", err)
	}
	if fresh, _ := vals[0].(bool); !fresh {
		t.Fatal("reloaded generation is not fresh")
	}
}

// TestQueuePendingDroppedOnReload verifies that requests queued behind a
// busy interpreter are answered with ErrReloaded when ForceReload retires
// the generation (DESIGN §4: reload purges every registration of the dead
// generation before replay), and the running one is canceled: left going,
// it would see the fresh generation's origin frames.
func TestQueuePendingDroppedOnReload(t *testing.T) {
	r := newTestRuntime(t)

	started := make(chan struct{})
	running := make(chan error, 1)
	go func() {
		_, err := r.Do(context.Background(), ClassIO, 10*time.Second, time.Second,
			func(L *glua.LState) ([]any, error) {
				close(started)
				return nil, L.DoString("while true do end")
			})
		running <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("sleeper never started")
	}

	pending := make(chan error, 1)
	go func() {
		_, err := r.Do(context.Background(), ClassUI, 0, 0,
			func(L *glua.LState) ([]any, error) { return nil, nil })
		pending <- err
	}()
	time.Sleep(50 * time.Millisecond)

	if err := r.ForceReload(context.Background(), nil, nil); err != nil {
		t.Fatalf("ForceReload: %v", err)
	}
	select {
	case err := <-pending:
		if !errors.Is(err, ErrReloaded) {
			t.Fatalf("pending request got %v, want ErrReloaded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending request never answered")
	}
	select {
	case err := <-running:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("running request got %v, want canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("running request kept going after the reload")
	}
}

// TestChunkAfterReload: a request still running when ForceReload retires its
// generation compiles nothing against the fresh sandbox or a nil one.
func TestChunkAfterReload(t *testing.T) {
	r := newTestRuntime(t)
	old := r.current().L
	if err := r.ForceReload(context.Background(), nil, nil); err != nil {
		t.Fatalf("ForceReload: %v", err)
	}
	if err := r.runChunk(old, "return 1", "late.lua", OriginUser); !errors.Is(err, ErrReloaded) {
		t.Fatalf("late chunk got %v, want ErrReloaded", err)
	}
}

// TestQueueForceReloadReplays verifies that a reload boots a fresh
// generation and replays the boot steps on it.
func TestQueueForceReloadReplays(t *testing.T) {
	r := NewRuntime(RuntimeOptions{})

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

	if err := r.Boot(context.Background(), steps); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	readMark := func() int64 {
		vals, err := r.Do(context.Background(), ClassUI, 0, 0,
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

	if err := r.ForceReload(context.Background(), steps, func() { purgeCount++ }); err != nil {
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
	r := newTestRuntime(t)

	started := make(chan struct{})
	go func() {
		_, _ = r.Do(context.Background(), ClassIO, 50*time.Millisecond, 50*time.Millisecond,
			func(L *glua.LState) ([]any, error) {
				close(started)
				time.Sleep(700 * time.Millisecond)
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
		_, err := r.Do(context.Background(), ClassUI, 0, 0,
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
}

// TestQueueIntegrityGoesHot: a frozen namespace mutated behind the
// sandbox fails its request and wedges the runtime, so a tampered surface
// never serves later calls.
func TestQueueIntegrityGoesHot(t *testing.T) {
	r := NewRuntime(RuntimeOptions{})
	var ns *glua.LTable
	step := BootStep{Name: "ns", Run: func(ctx context.Context, rt *Runtime, g *Generation) error {
		ns = g.L.NewTable()
		ns.RawSetString("fn", g.L.NewFunction(func(*glua.LState) int { return 0 }))
		return g.SB.RegisterNamespace("zemi.ui", ns)
	}}
	if err := r.Boot(context.Background(), []BootStep{step}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	noop := func(L *glua.LState) ([]any, error) { return nil, nil }
	if _, err := r.Do(context.Background(), ClassUI, 0, 0, noop); err != nil {
		t.Fatalf("clean request: %v", err)
	}
	_, err := r.Do(context.Background(), ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
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

// TestEvalKeepsProtectedGlobalGuard: the lua tool runs through Eval in a
// wrapper env, which must keep the chunk env's __newindex guard so
// assigning a protected global raises instead of silently shadowing it.
func TestEvalKeepsProtectedGlobalGuard(t *testing.T) {
	r := NewRuntime(RuntimeOptions{})
	step := BootStep{Name: "ns", Run: func(ctx context.Context, rt *Runtime, g *Generation) error {
		tbl := g.L.NewTable()
		tbl.RawSetString("fn", g.L.NewFunction(func(*glua.LState) int { return 0 }))
		return g.SB.RegisterNamespace("zemi.ui", tbl)
	}}
	if err := r.Boot(context.Background(), []BootStep{step}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	discard := func(string) {}
	if _, err := r.Eval(context.Background(), `zemi = { ui = false }`, "lua-tool", OriginLearned, time.Second, discard); err == nil {
		t.Fatal("assignment to the protected zemi global was accepted")
	}
	vals, err := r.Eval(context.Background(), `x = 5 return x`, "lua-tool", OriginLearned, time.Second, discard)
	if err != nil || len(vals) != 1 {
		t.Fatalf("ordinary global in the wrapper env: %v (%v)", err, vals)
	}
}

// TestQueueDoBeforeBoot fails fast instead of panicking.
func TestQueueDoBeforeBoot(t *testing.T) {
	r := NewRuntime(RuntimeOptions{})
	_, err := r.Do(context.Background(), ClassUI, 0, 0,
		func(L *glua.LState) ([]any, error) { return nil, nil })
	if err == nil || err.Error() != "zemi: lua runtime not booted" {
		t.Fatalf("expected not-booted error, got %v", err)
	}
}
