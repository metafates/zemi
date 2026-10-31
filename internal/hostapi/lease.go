// lease.go is the writable window: one row in the shared store names the window
// that may write this project's chat, and every other window is a read-only
// viewer. A window claims the row at boot, before its first prompt; the holder
// renews its beat on the peers tick, and a survivor takes the row over once the
// beat goes stale or a clean close releases it. Presence says who is watching;
// the lease says who writes.

package hostapi

import (
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/metafates/zemi/internal/store"
)

// The lease word the ctx and the statusline read.
const (
	writerThis  = "this"
	writerOther = "other"
	writerNone  = "none"
)

// errNotWriter is the one refusal a viewer's every write path names.
var errNotWriter = errors.New("another window writes this chat")

// claimLease takes the lease for this window at boot: the first over an absent
// row wins, and a row that names a window - however stale - leaves this one a
// viewer; the ticker's takeover wins a stale row once the beat has seen it. A
// store too busy to answer is a wait, not a verdict: this window keeps the
// write, and the first renew sorts the row out.
func (h *Host) claimLease() {
	won, err := h.DB.ClaimLeader(h.origin, h.sockPath, store.Leader{})
	if err != nil {
		h.Log.Warn("lease not claimed", "origin", "lease", "error", err)
		return
	}
	if !won {
		h.setWriter(false)
		h.Log.Info("another window writes this chat", "origin", "lease")
	}
}

// leaseTick renews the lease this window holds, or takes over one whose holder
// stopped renewing, and stores the word the pill reads. A store error is a
// wait, not a verdict: only a clean loss - zero rows, no error - ends this
// window's write, so SQLITE_BUSY never flaps a healthy leader mid-turn.
func (h *Host) leaseTick() {
	if h.DB == nil || h.peersPath == "" {
		return
	}
	if h.writerFlag() {
		ok, err := h.DB.RenewLeader(h.origin)
		if err != nil {
			h.Log.Warn("lease not renewed", "origin", "lease", "error", err)
			return
		}
		if !ok {
			h.demote()
		}
		return
	}
	l, err := h.DB.ReadLeader()
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			h.Log.Warn("lease not read", "origin", "lease", "error", err)
			return
		}
		l = store.Leader{} // no holder: the first claim wins
	} else if !l.Stale(h.staleAfter()) && l.Origin != h.origin {
		h.setLeaseWord(writerOther) // a live holder
		return
	}
	won, err := h.DB.ClaimLeader(h.origin, h.sockPath, l)
	if err != nil {
		h.Log.Warn("lease not taken over", "origin", "lease", "error", err)
		if l.Origin == "" {
			h.setLeaseWord(writerNone) // no holder the claim could reach
		}
		return
	}
	if !won {
		h.setLeaseWord(writerOther)
		return
	}
	h.promote()
}

// promote hands this window the write after a takeover: the reload runs first,
// so the flag is never visible before the transcript holds the previous
// writer's rows, and the goal the reload read is this window's chain to run.
func (h *Host) promote() {
	h.SyncChat()
	h.loadGoal()
	h.setWriter(true)
	h.Notify(slog.LevelInfo, "This window writes the chat now", "the window that wrote it is gone")
}

// demote ends this window's write after a clean loss. The flag flips before the
// interrupt, or the same tick's nextTurn would start a goal chain this window
// no longer owns. The turn's logged rows stay - the interrupt notice is the
// honest end - the steering queue returns to the composer, and any loop stops:
// a viewer's timer would send into the refusal every interval.
func (h *Host) demote() {
	h.setWriter(false)
	h.AgentCancel()
	_ = h.stopLoop(nil, "Loop stopped: "+errNotWriter.Error())
	if q := h.AgentTakeQueue(); len(q) > 0 {
		if ui := h.hooks(); ui != nil {
			ui.ComposerEdit(ComposerSet, strings.Join(q, "\n"), -1)
		}
	}
	h.Notify(slog.LevelWarn, "Another window writes the chat", "this window is a viewer now; its drafts are kept")
}

// releaseLease drops the lease on a clean close, so a survivor promotes in one
// tick instead of one TTL. A viewer releases nothing; presence off (headless,
// tests) never held one.
func (h *Host) releaseLease() {
	if h.DB == nil || h.peersPath == "" || !h.writerFlag() {
		return
	}
	if _, err := h.DB.ReleaseLeader(h.origin); err != nil {
		h.Log.Warn("lease not released", "origin", "lease", "error", err)
	}
}

// writerFlag reports whether this window writes the chat.
func (h *Host) writerFlag() bool {
	h.agents.mu.Lock()
	defer h.agents.mu.Unlock()
	return h.agents.writer
}

// setWriter flips the flag under the agents' lock and stores the word with it,
// so the gate and the pill never disagree.
func (h *Host) setWriter(mine bool) {
	ag := h.agents
	ag.mu.Lock()
	ag.writer = mine
	ag.mu.Unlock()
	h.setLeaseWord(map[bool]string{true: writerThis, false: writerOther}[mine])
}

// setLeaseWord stores what the pill and the ctx read.
func (h *Host) setLeaseWord(w string) { h.lease.Store(&w) }

// WriterWord names the lease for the ctx and the statusline: this window
// writes the chat, another one does, or none does yet. A host with no presence
// (headless, tests) is its store's one window.
func (h *Host) WriterWord() string {
	if w := h.lease.Load(); w != nil {
		return *w
	}
	return writerThis
}

// IsViewer reports whether another window writes this chat: the composer's
// placeholder reads it.
func (h *Host) IsViewer() bool { return h.WriterWord() == writerOther }

// staleAfter is how old the holder's beat may be before a survivor takes the
// lease over; 0 reads peersTTL, and a test narrows it to play a dead holder
// without waiting one out.
func (h *Host) staleAfter() time.Duration {
	if h.leaseTTL > 0 {
		return h.leaseTTL
	}
	return peersTTL
}
