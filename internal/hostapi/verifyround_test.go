// Attacks against the session lock order and the goal kv: each test names
// the interleave it runs, and the race ones are evidence only under -race.

package hostapi

import (
	"encoding/json/v2"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
)

// TestVerifyLockOrderUnderTurnContention attacks the lock-order invariant
// with everything the pins leave out at once: a real turn start and its
// stream flushes, a step retry, a late reasoning join,
// settled appends and the read paths. A deadlock hangs the
// test; the race detector judges the memory ordering.
func TestVerifyLockOrderUnderTurnContention(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	h, _ := newWindow(t, state, proj)
	var appendedSettled, appendedLive atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	spin := func(fn func(i int)) {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				fn(i)
			}
		})
	}
	spin(func(i int) {
		h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: fmt.Sprintf("s %d", i)})
		appendedSettled.Add(1)
	})
	spin(func(i int) {
		ag := h.agents
		ag.mu.Lock()
		ag.textBuf = "live"
		ag.mu.Unlock()
		h.appendAfterLive(ChatMessage{Role: fantasy.MessageRoleAssistant, Text: fmt.Sprintf("l %d", i)})
		appendedLive.Add(1)
	})
	spin(func(int) { h.cutAssistantRows(len(h.Session().Transcript) / 2) })
	spin(func(int) { h.joinReasoning(" more") })
	spin(func(int) {
		_ = h.Session()
		_ = h.AgentStream()
		_ = h.chatWindowLo()
		_, _ = h.Goal()
	})
	spin(func(i int) {
		g := Goal{Condition: fmt.Sprintf("g %d", i), SetAt: int64(i)}
		h.agents.mu.Lock()
		h.agents.goal = &g
		h.agents.mu.Unlock()
		h.storeGoal(&g)
		h.loadGoal()
	})
	spin(func(i int) {
		if h.AgentActive() {
			time.Sleep(time.Millisecond)
			return
		}
		_ = h.AgentSend(fmt.Sprintf("turn %d", i))
		time.Sleep(2 * time.Millisecond)
	})
	// a hang names its owner: dump every goroutine's stack once. The watch
	// dies with the test, so a fast run's watch never errors into a later
	// one - the panic on a log after the test completed.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-time.After(45 * time.Second):
		case <-watchDone:
			return
		}
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		t.Errorf("hung: %s", buf)
		close(stop)
	}()
	time.Sleep(5 * time.Second)
	close(stop)
	wg.Wait()
	tr := h.Session().Transcript
	if len(tr) > _chatWindowRows {
		t.Fatalf("the window holds %d rows, bound %d", len(tr), _chatWindowRows)
	}
	if total := appendedSettled.Load() + appendedLive.Load(); total > 2*_chatWindowRows && h.trCut.Load() == 0 {
		t.Fatalf("no trim ran in %d appends", total)
	}
}

// TestVerifyUnstoreGoalTOCTOU attacks the stale-goal delete's check and
// delete being two store calls with no lock spanning them: a replacement
// written between them dies to the delete. The writer re-asserts its fresh
// row; the row going missing right after a write is the interleave hit.
func TestVerifyUnstoreGoalTOCTOU(t *testing.T) {
	h, _ := newWindow(t, t.TempDir(), t.TempDir())
	stale := Goal{Condition: "goal one", SetAt: 100}
	row := func(setAt int64) string {
		raw, _ := json.Marshal(Goal{Condition: "goal one", SetAt: setAt}, json.Deterministic(true))
		return string(raw)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(4)
	for range 4 { // four stale copies' MET paths race the writer
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				h.unstoreGoal(&stale)
			}
		}()
	}
	lost := ""
	for i := 0; i < 4000 && lost == ""; i++ {
		if err := h.DB.SetKV(_goalKV, row(101)); err != nil {
			t.Fatal(err)
		}
		runtime.Gosched() // the deleter reads goal one here
		if err := h.DB.SetKV(_goalKV, row(102)); err != nil {
			t.Fatal(err)
		}
		if raw, err := h.DB.GetKV(_goalKV); err != nil || raw != row(102) {
			lost = fmt.Sprintf("round %d: the writer's row(102) is %q (%v) after its write", i, raw, err)
		}
	}
	close(stop)
	wg.Wait()
	if lost != "" {
		t.Fatalf("the stale copy deleted the writer's replacement: %s", lost)
	}
}
