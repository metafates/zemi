// safe.go keeps a panic in a host goroutine from killing zemi: the
// goroutines the host starts recover, log the stack to the run's log file,
// and surface the panic in /errors while the app keeps running.

package hostapi

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"charm.land/fantasy"
)

// errPanicked is the failure an async handle's wait reads when the goroutine
// running the work panicked before the real result landed: GoSafe logs the
// panic, and the wait ends with this instead of an empty success or its cap.
var errPanicked = errors.New("the call panicked")

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
// the turn and clears its live state, so the agent is not stuck "active" with
// nothing running. The panic is logged, then the app goes on.
func (h *Host) endTurnAfterPanic() {
	if r := recover(); r != nil {
		h.clearCrashedTurn(r)
		h.Alert(slog.LevelError, "Turn crashed", "it was stopped; /errors has the cause")
		h.refresh()
	}
}

// clearCrashedTurn logs a turn's panic and lets the agent go idle.
func (h *Host) clearCrashedTurn(r any) {
	h.logPanic("agent turn", r)
	ag := h.agents
	ag.mu.Lock()
	h.release()
	ag.checkingGoal = false
	ag.textBuf, ag.reasonBuf, ag.tools = "", "", nil
	ag.turnStart, ag.retryAt = time.Time{}, time.Time{}
	ag.mu.Unlock()
	// the tail must terminate: a panic left unlogged reads as a turn the
	// window closed on, with the reply the user read gone
	row := ChatMessage{
		Role: fantasy.MessageRoleAssistant, Kind: RowError,
		Text: "the turn died with an internal error", Status: CallCancelled,
	}
	if h.chat != nil {
		if err := h.chat.Error(row.Text, true); err != nil {
			h.logAppendFailed(err)
		}
	}
	h.AppendChat(row)
}

// logPanic records a recovered panic: full stack in the log, one line in
// /errors.
func (h *Host) logPanic(where string, r any) {
	h.Log.Error(fmt.Sprintf("recovered panic in %s: %v", where, r), "origin", "panic",
		"stack", string(debug.Stack()),
	)
	h.RecordError("panic", where, fmt.Sprint(r))
}
