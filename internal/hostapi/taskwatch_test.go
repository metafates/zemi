package hostapi

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestOverlayTicker: a screen of a running thing is rebuilt every tick until
// more says nothing moves, so an overlay whose rows are a snapshot keeps up.
func TestOverlayTicker(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	h.SetUIHooks(&fakeUI{})
	var n atomic.Int32
	done := make(chan int32, 1)
	h.tickOverlay(time.Millisecond, "test view", nil, func() bool {
		if n.Add(1) == 3 {
			done <- n.Load()
			return false
		}
		return true
	})
	select {
	case got := <-done:
		if got != 3 {
			t.Fatalf("ticks = %d", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the ticker never reached its last pass")
	}
	// one ticker per screen: a second caller finds the claim taken
	if !h.claimAgentsView() || h.claimAgentsView() {
		t.Fatal("two tickers claimed the agents view")
	}
}
