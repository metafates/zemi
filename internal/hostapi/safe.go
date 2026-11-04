// safe.go keeps a panic in a host goroutine from killing zemi: the
// goroutines the host starts recover, log the stack to the run's log file,
// and surface the panic in /errors while the app keeps running. It counts
// them too, so Close waits for the ones in flight.

package hostapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"charm.land/fantasy"
)

// errPanicked is the failure an async handle's wait reads when the goroutine
// running the work panicked before the real result landed: GoSafe logs the
// panic, and the wait ends with this instead of an empty success or its cap.
var errPanicked = errors.New("the call panicked")

// closeWait bounds Close's wait for the goroutines GoSafe started: one let go
// by the runtime's close unwinds in milliseconds, and a wedged one costs the
// shutdown a pause, never a hang.
var closeWait = 5 * time.Second

func (h *Host) GoSafe(where string, fn func()) {
	h.goes.add()
	go func() {
		defer h.goes.done()
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

// goes counts the goroutines GoSafe runs: every async callback and handler
// reaches Lua through one, so this is the work a host has in flight.
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
	// a goroutine quietFrom left out of the count ends with the host
	if g.n > 0 {
		if g.n--; g.n == 0 {
			close(g.none)
		}
	}
	g.mu.Unlock()
}

// quietFrom counts from nothing now: the goroutines running are the host's own
// services, which end with it and are joined by Close anyway, not work in
// flight. Only valid while every one of them is such a service.
func (g *goes) quietFrom() {
	g.mu.Lock()
	g.n, g.none = 0, nil
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
// the turn and clears its live state, so the agent is not stuck "active" with
// nothing running. The panic is logged, then the app goes on.
func (h *Host) endTurnAfterPanic() {
	if r := recover(); r != nil {
		h.clearCrashedTurn(r)
		h.Alert(slog.LevelError, "Turn crashed", "zemi kept running; the cause is in /errors")
		h.refresh()
	}
}

// clearCrashedTurn logs a turn's panic and lets the agent go idle.
func (h *Host) clearCrashedTurn(r any) {
	h.logPanic("agent turn", r)
	ag := h.agents
	ag.mu.Lock()
	ag.checkingGoal = false
	ag.textBuf, ag.reasonBuf, ag.tools = "", "", nil
	ag.turnStart, ag.retryAt = time.Time{}, time.Time{}
	ag.promptWait = 0
	h.release()
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
