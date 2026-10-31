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

	"github.com/metafates/zemi/internal/ipc"
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
// handed says the takeover answers a hand-over rather than a death, so the
// promotion names which.
func (h *Host) leaseTick(handed bool) {
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
		if h.queueWaiting() {
			h.requestWrite() // a parked message keeps asking for the write
		}
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
	h.promote(handed)
}

// promote hands this window the write after a takeover: the reload runs first,
// so the flag is never visible before the transcript holds the previous
// writer's rows, and the goal the reload read is this window's chain to run.
// A message parked for the write sends now.
func (h *Host) promote(handed bool) {
	h.SyncChat()
	if !handed {
		// the write came from a holder that died inside its turn: say its turn
		// ended the way it did. A hand-over ended its turn cleanly at the
		// boundary and marks nothing.
		h.markDanglingTail()
	}
	h.loadGoal()
	h.setWriter(true)
	detail := "the window that wrote it is gone"
	if handed {
		detail = "the writing window handed it over"
	}
	h.Notify(slog.LevelInfo, "This window writes the chat now", detail)
	h.drainQueue()
}

// demote ends this window's write after a clean loss. The flag flips before the
// interrupt, or the same tick's nextTurn would start a goal chain this window
// no longer owns. The turn's logged rows stay - the interrupt notice is the
// honest end - the steering queue returns to the composer, and any loop stops:
// a viewer's timer would send into the refusal every interval.
func (h *Host) demote() {
	ag := h.agents
	ag.mu.Lock()
	ag.yield = nil // a parked ask died with the write
	ag.mu.Unlock()
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

// yieldAsk is a viewer's ask for the write: the window that asked, and the
// socket the hand-over pokes.
type yieldAsk struct{ origin, sock string }

// parkForHandover queues a typed message on a window that does not write, and
// asks the holder to hand the chat over. The queue is live-only, like a draft:
// it goes with this window, and the write's arrival - a hand-over, a takeover,
// a kill - sends it.
func (h *Host) parkForHandover(text string) {
	ag := h.agents
	ag.mu.Lock()
	ag.steering = append(ag.steering, text)
	ag.mu.Unlock()
	h.refresh()
	h.requestWrite()
	h.Notify(slog.LevelInfo, "Queued for the write",
		"the writing window hands the chat over at its next step; /windows kill takes it sooner")
}

// requestWrite asks the window that holds the write to hand it over. It is
// fire-and-forget: the beat asks again while the queue waits, so an ask that
// missed its holder - it died, it was killed, the row moved - finds the next.
func (h *Host) requestWrite() {
	if h.DB == nil || h.peersPath == "" {
		return
	}
	h.GoSafe("write ask", func() {
		l, err := h.DB.ReadLeader()
		if err != nil || l.Origin == h.origin || l.Sock == "" {
			return
		}
		conn, err := ipc.Dial(l.Sock, time.Second)
		if err != nil {
			return
		}
		_, _ = ipc.RoundTrip(conn, &ipc.Request{Yield: true, Origin: h.origin, Sock: h.sockPath}, 2*time.Second)
		_ = conn.Close()
	})
}

// yieldWrite is the ask as its socket delivered it. A writer mid-turn parks it
// - the runner's Full ends the turn at its next tool boundary, where nextTurn
// hands the chat over - and an idle writer hands it over at once. The first
// ask while one waits wins; a second asker's beat asks the next holder.
func (h *Host) yieldWrite(origin, sock string) {
	if origin == "" || sock == "" || !h.writerFlag() {
		return
	}
	ag := h.agents
	ag.mu.Lock()
	if ag.active {
		if ag.yield == nil {
			ag.yield = &yieldAsk{origin: origin, sock: sock}
			ag.mu.Unlock()
			h.Notify(slog.LevelInfo, "A window asks for the write",
				"this turn ends at its next step and the chat is handed over")
		} else {
			ag.mu.Unlock()
		}
		return
	}
	ag.mu.Unlock()
	h.handOver(yieldAsk{origin: origin, sock: sock})
}

// handOver writes the lease to the asking window, demotes this one to a
// viewer, and pokes the asker so it promotes now instead of at its next beat.
// The claim is a CAS on this window's own row: a renewal that landed in
// between loses the ask nothing - the asker's next beat asks again.
func (h *Host) handOver(y yieldAsk) {
	l, err := h.DB.ReadLeader()
	if err != nil || l.Origin != h.origin {
		return // not this window's to give
	}
	won, err := h.DB.ClaimLeader(y.origin, y.sock, l)
	if err != nil {
		h.Log.Warn("lease not handed over", "origin", "lease", "error", err)
		return
	}
	if !won {
		return
	}
	h.demote()
	h.pokeYours(y.sock)
}

// pokeYours tells the window the write was handed to, so it reads the lease
// now. A poke that misses costs nothing: the ticker sweeps it.
func (h *Host) pokeYours(sock string) {
	h.GoSafe("yours poke", func() {
		conn, err := ipc.Dial(sock, time.Second)
		if err != nil {
			return
		}
		_, _ = ipc.RoundTrip(conn, &ipc.Request{Yours: true}, 2*time.Second)
		_ = conn.Close()
	})
}

// queueWaiting reports whether a typed message parks for the write this
// window does not hold.
func (h *Host) queueWaiting() bool {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return len(ag.steering) > 0
}

// yieldPending reports whether a viewer's ask waits for this turn's next step
// boundary; the runner's Full reads it.
func (h *Host) yieldPending() bool {
	ag := h.agents
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return ag.yield != nil
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
	ag.remote = nil // the write and the watch never share a window
	ag.mu.Unlock()
	word := writerOther
	if mine {
		word = writerThis
	}
	h.setLeaseWord(word)
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
