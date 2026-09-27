// safe.go keeps a panic in a host goroutine from killing zemi: the
// goroutines the host starts recover, log the stack to the run's log file,
// and surface the panic in /errors while the app keeps running.
package hostapi

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/metafates/zemi/internal/logging"
)

func (h *Host) GoSafe(where string, fn func()) {
	h.goes.add()
	go func() {
		defer h.goes.done()
		defer func() {
			if r := recover(); r != nil {
				h.logPanic(where, r)
			}
		}()
		fn()
	}()
}

// goes counts the goroutines GoSafe runs: every async callback, handler and
// timer reaches Lua through one, so a test zemi is quiet when none runs.
type goes struct {
	mu   sync.Mutex
	n    int
	none chan struct{} // closed when n drops to zero
}

func (g *goes) add() {
	g.mu.Lock()
	if g.n == 0 {
		g.none = make(chan struct{})
	}
	g.n++
	g.mu.Unlock()
}

func (g *goes) done() {
	g.mu.Lock()
	if g.n--; g.n == 0 {
		close(g.none)
	}
	g.mu.Unlock()
}

// wait returns once no GoSafe goroutine runs, or with ctx's error.
func (g *goes) wait(ctx context.Context) error {
	g.mu.Lock()
	n, none := g.n, g.none
	g.mu.Unlock()
	if n == 0 {
		return nil
	}
	select {
	case <-none:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// endTurnAfterPanic is deferred in turn goroutines: a panic mid-turn stops
// the turn and clears its live state, so the agent is not stuck "active"
// with nothing running. The panic is logged, then the app goes on.
func (h *Host) endTurnAfterPanic() {
	r := recover()
	if r == nil {
		return
	}
	h.logPanic("agent turn", r)
	ag := h.agents
	ag.mu.Lock()
	ag.active, ag.compacting = false, false
	ag.textBuf, ag.reasonBuf, ag.tools = "", "", nil
	ag.turnStart, ag.retryAt = time.Time{}, time.Time{}
	ag.mu.Unlock()
	h.Notify(logging.Error, "Turn crashed", "it was stopped; /errors has the cause")
	h.refresh()
}

// logPanic records a recovered panic: full stack in the log, one line in
// /errors.
func (h *Host) logPanic(where string, r any) {
	h.Log.Error("panic", fmt.Sprintf("recovered panic in %s: %v", where, r), map[string]any{
		"stack": string(debug.Stack()),
	})
	h.RecordError("panic "+where, fmt.Sprint(r))
}
