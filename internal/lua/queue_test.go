package lua

import (
	"context"
	"errors"
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
		mu.Lock()
		order = append(order, "io:sleep")
		mu.Unlock()
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
	want := []string{"io:sleep", "ui", "agent", "io"}
	if len(order) != len(want) {
		t.Fatalf("execution order %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("execution order %v, want %v", order, want)
		}
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
// generation before replay).
func TestQueuePendingDroppedOnReload(t *testing.T) {
	r := newTestRuntime(t)

	started := make(chan struct{})
	go func() {
		_, _ = r.Do(context.Background(), ClassIO, 0, 0,
			func(L *glua.LState) ([]any, error) {
				close(started)
				time.Sleep(400 * time.Millisecond)
				return nil, nil
			})
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

// TestQueueDoBeforeBoot fails fast instead of panicking.
func TestQueueDoBeforeBoot(t *testing.T) {
	r := NewRuntime(RuntimeOptions{})
	_, err := r.Do(context.Background(), ClassUI, 0, 0,
		func(L *glua.LState) ([]any, error) { return nil, nil })
	if err == nil || err.Error() != "zemi: lua runtime not booted" {
		t.Fatalf("expected not-booted error, got %v", err)
	}
}
