// Attacks against the shared-session lock order and the goal handoff: each
// test names the interleave it runs, and the race ones are evidence only
// under -race.

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

// TestVerifySnapshotRacesTheHeldDrop attacks the drop's atomicity claim
// from outside the lock: Session() hands widgets a snapshot that shares the
// transcript's backing array ("never written in place", host.go), but
// dropHeldRow shifts that array in place. A render that snapshotted before
// the drop reads shifted rows after it.
func TestVerifySnapshotRacesTheHeldDrop(t *testing.T) {
	h, _ := newTestHost(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	torn := make(chan string, 1)
	go func() { // a widget render: snapshot under RLock, read outside it
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			snap := h.Session()
			n := len(snap.Transcript)
			if n >= 2 && snap.Transcript[n-1].Text != "" && snap.Transcript[n-1].Text == snap.Transcript[n-2].Text {
				select {
				case torn <- fmt.Sprintf("snapshot rows %d/%d both %q", n-2, n-1, snap.Transcript[n-1].Text):
				default:
				}
			}
		}
	}()
	go func() { // esc of a held prompt: its row lands, then the drop
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			text := fmt.Sprintf("held %d", i)
			held := &heldPrompt{text: text}
			h.tMu.RLock()
			held.base = len(h.session.Transcript)
			held.cut = h.trCut.Load()
			h.tMu.RUnlock()
			h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: text})
			h.dropHeldRow(held)
		}
	}()
	time.Sleep(700 * time.Millisecond)
	close(stop)
	wg.Wait()
	select {
	case s := <-torn:
		t.Fatalf("a snapshot read the drop's in-place shift: %s", s)
	default:
	}
}

// TestVerifySnapshotRacesTheForeignSplice runs the attack against the sync
// tick's splice: a foreign row whose seq sits before this window's stamped
// tail inserts mid-array, and slices.Insert shifts the shared backing array
// in place when its capacity allows - while Session() snapshots read it
// unlocked. b runs its own turns so its rows stamp and the splice lands
// mid-array; a's rows are older by the time b's tick sees them.
func TestVerifySnapshotRacesTheForeignSplice(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a, _ := newWindow(t, state, proj)
	b, _ := newWindow(t, state, proj)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // b's render
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = hRowTexts(b.Session())
		}
	}()
	go func() { // b drives itself: its own stamped turns, and the sync tick
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			b.SyncChat()
			if !b.AgentActive() && i%2 == 0 {
				_ = b.AgentSend(fmt.Sprintf("b own %d", i))
			}
			time.Sleep(time.Millisecond)
		}
	}()
	go func() { // a writes through its turns, the only path into the log
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if !a.AgentActive() {
				_ = a.AgentSend(fmt.Sprintf("foreign %d", i))
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	time.Sleep(2 * time.Second)
	close(stop)
	wg.Wait()
	b.SyncChat()
	if n := len(b.Session().Transcript); n < 4 {
		t.Fatalf("b holds %d rows: neither window's turns nor the splice ran", n)
	}
}

func hRowTexts(s Session) []string {
	out := make([]string, len(s.Transcript))
	for i, m := range s.Transcript {
		out[i] = m.Text
	}
	return out
}

// TestVerifyLockOrderUnderTurnContention attacks the lock-order invariant
// with everything the pins leave out at once: a real turn start and its
// stream flushes, a hold, a held drop, a step retry, a late reasoning join,
// the sync tick, settled appends and the read paths. A deadlock hangs the
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
	spin(func(int) { h.SyncChat() })
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
	spin(func(i int) {
		text := fmt.Sprintf("h %d", i)
		held := &heldPrompt{text: text}
		h.tMu.RLock()
		held.base, held.cut = len(h.session.Transcript), h.trCut.Load()
		h.tMu.RUnlock()
		h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: text})
		h.dropHeldRow(held)
	})
	spin(func(int) {
		_ = h.Session()
		_ = h.AgentStream()
		_ = h.chatJumpRows()
		_, _ = h.Goal()
	})
	spin(func(i int) {
		g := Goal{Condition: fmt.Sprintf("g %d", i), SetAt: int64(i), Writer: h.origin}
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
	// a hang names its owner: dump every goroutine's stack once
	go func() {
		<-time.After(45 * time.Second)
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		t.Errorf("hung: %s", buf)
		close(stop)
	}()
	time.Sleep(5 * time.Second)
	close(stop)
	wg.Wait()
	tr := h.Session().Transcript
	if len(tr) > chatWindowRows {
		t.Fatalf("the window holds %d rows, bound %d", len(tr), chatWindowRows)
	}
	if total := appendedSettled.Load() + appendedLive.Load(); total > 2*chatWindowRows && h.trCut.Load() == 0 {
		t.Fatalf("no trim ran in %d appends", total)
	}
}

// TestVerifyUnstoreGoalTOCTOU attacks the stale-goal delete's check and
// delete being two store calls with no lock spanning them: a replacement
// written between them dies to the delete. The writer re-asserts its fresh
// row; the row going missing right after a write is the interleave hit.
func TestVerifyUnstoreGoalTOCTOU(t *testing.T) {
	h, _ := newWindow(t, t.TempDir(), t.TempDir())
	stale := Goal{Condition: "goal one", SetAt: 100, Writer: "a"}
	row := func(setAt int64) string {
		raw, _ := json.Marshal(Goal{Condition: "goal one", SetAt: setAt, Writer: "a"}, json.Deterministic(true))
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
		if err := h.DB.SetKV(goalKV, row(101)); err != nil {
			t.Fatal(err)
		}
		runtime.Gosched() // the deleter reads goal one here
		if err := h.DB.SetKV(goalKV, row(102)); err != nil {
			t.Fatal(err)
		}
		if raw, err := h.DB.GetKV(goalKV); err != nil || raw != row(102) {
			lost = fmt.Sprintf("round %d: the writer's row(102) is %q (%v) after its write", i, raw, err)
		}
	}
	close(stop)
	wg.Wait()
	if lost != "" {
		t.Fatalf("the stale copy deleted the writer's replacement: %s", lost)
	}
}

// TestVerifyHoldPairUnderTrim attacks the hold's (base, cut) pair and the
// drop's targeting under head churn: both reads share one tMu.RLock and the
// trim takes the write lock, so the drop may only ever remove the hold's
// own row, and a shed row must remove nothing at all.
func TestVerifyHoldPairUnderTrim(t *testing.T) {
	h, _ := newTestHost(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() { // the settled append's trim churns the head
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: fmt.Sprintf("filler %d", i)})
		}
	})
	time.Sleep(50 * time.Millisecond)
	for i := range 500 {
		text := fmt.Sprintf("held %d", i)
		held := &heldPrompt{text: text}
		h.tMu.RLock()
		held.base, held.cut = len(h.session.Transcript), h.trCut.Load()
		h.tMu.RUnlock()
		h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: text})
		before := h.Session()
		counts := map[string]int{}
		for _, m := range before.Transcript {
			if m.Text != text {
				counts[m.Text]++
			}
		}
		h.dropHeldRow(held)
		after := h.Session()
		// new filler rows land between the two reads, and the trim sheds a
		// prefix of the fillers; any other disappearance is the drop taking
		// a row it does not own
		seen := map[string]int{}
		for _, m := range after.Transcript {
			seen[m.Text]++
		}
		removed := map[int]bool{}
		for s, n := range counts {
			if seen[s] >= n {
				continue
			}
			var k int
			if _, err := fmt.Sscanf(s, "filler %d", &k); err != nil {
				continue // a held row a trim finally shed, or the round's own
			}
			removed[k] = true
		}
		// the trim sheds fillers from the oldest present upward; a removed
		// filler past that prefix is a middle row the drop took
		oldest := -1
		for s := range counts {
			var k int
			if _, err := fmt.Sscanf(s, "filler %d", &k); err == nil && (oldest < 0 || k < oldest) {
				oldest = k
			}
		}
		prefix := 0
		if oldest >= 0 {
			for removed[oldest+prefix] {
				prefix++
			}
		}
		for k := range removed {
			if k < oldest+prefix {
				continue
			}
			t.Fatalf("round %d: filler %d vanished while older filler %d stands: a middle row died to the drop", i, k, oldest+prefix)
		}
	}
	close(stop)
	wg.Wait()
}
