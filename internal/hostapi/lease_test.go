package hostapi

import (
	"testing"
	"time"
)

// openClaimed boots a window on the shared state and opens its presence, so
// the lease decides whether it writes - the way two zemi processes on one
// repository see it.
func openClaimed(t *testing.T, stateHome, projectDir string) *Host {
	t.Helper()
	h, _ := newWindow(t, stateHome, projectDir)
	h.OpenPeers()
	t.Cleanup(h.ClosePeers)
	return h
}

// TestLeaseOneWindowWrites: the first window over the store claims the lease,
// a second opens as a viewer, a live holder keeps it, and a clean close hands
// the write to the survivor on its next tick.
func TestLeaseOneWindowWrites(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a := openClaimed(t, state, proj)
	if !a.writerFlag() || a.WriterWord() != writerThis {
		t.Fatalf("the first window writes: writer=%v word=%q", a.writerFlag(), a.WriterWord())
	}
	b := openClaimed(t, state, proj)
	if b.writerFlag() || b.WriterWord() != writerOther {
		t.Fatalf("the second window views: writer=%v word=%q", b.writerFlag(), b.WriterWord())
	}

	b.leaseTick() // a live holder's beat is fresh: the viewer leaves it alone
	if b.writerFlag() {
		t.Fatal("the viewer took the lease from a live holder")
	}

	a.ClosePeers() // a clean close releases, so the survivor needs no TTL
	b.leaseTick()
	if !b.writerFlag() || b.WriterWord() != writerThis {
		t.Fatalf("the survivor writes after the holder's close: writer=%v word=%q", b.writerFlag(), b.WriterWord())
	}
}

// TestLeaseTTLTakeoverAndSelfDemotion: a holder that died without closing is
// taken over once its beat goes stale, and the zombie - still renewing from
// its own clock - steps down on the tick its renew comes back empty.
func TestLeaseTTLTakeoverAndSelfDemotion(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a := openClaimed(t, state, proj)
	b := openClaimed(t, state, proj)
	b.leaseTTL = time.Nanosecond // play a's death: its beat is older than this

	b.leaseTick()
	if !b.writerFlag() || b.WriterWord() != writerThis {
		t.Fatalf("the survivor took over the stale lease: writer=%v word=%q", b.writerFlag(), b.WriterWord())
	}

	a.leaseTick()
	if a.writerFlag() || a.WriterWord() != writerOther {
		t.Fatalf("the replaced holder demoted: writer=%v word=%q", a.writerFlag(), a.WriterWord())
	}
}

// TestLeaseRenewErrorKeepsTheWrite: a store that cannot answer is a wait, not
// a verdict - the holder keeps the lease rather than demoting on SQLITE_BUSY.
func TestLeaseRenewErrorKeepsTheWrite(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a := openClaimed(t, state, proj)
	if err := a.DB.Close(); err != nil {
		t.Fatal(err)
	}
	a.leaseTick()
	if !a.writerFlag() || a.WriterWord() != writerThis {
		t.Fatalf("an unreachable store demoted the holder: writer=%v word=%q", a.writerFlag(), a.WriterWord())
	}
}
