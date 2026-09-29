// safe.go keeps a panic in a host goroutine from killing zemi: the
// goroutines the host starts recover, log the stack to the run's log file,
// and surface the panic in /errors while the app keeps running.
package hostapi

import (
	"fmt"
	"runtime/debug"
	"time"

	"github.com/metafates/zemi/internal/logging"
)

func (h *Host) GoSafe(where string, fn func()) {
	t := h.test
	if t != nil {
		t.goes.add()
	}
	go func() {
		if t != nil {
			defer t.goes.done()
		}
		h.safe(where, fn)
	}()
}

// safe runs fn on this goroutine, a panic logged as GoSafe logs it.
func (h *Host) safe(where string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			h.logPanic(where, r)
		}
	}()
	fn()
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
	ag.active, ag.compacting, ag.checkingGoal = false, false, false
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
