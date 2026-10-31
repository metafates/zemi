package hostapi

import (
	"errors"
	"slices"
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
	if b.writerFlag() || b.WriterWord() != writerOther || !b.IsViewer() {
		t.Fatalf("the second window views: writer=%v word=%q viewer=%v", b.writerFlag(), b.WriterWord(), b.IsViewer())
	}

	b.leaseTick() // a live holder's beat is fresh: the viewer leaves it alone
	if b.writerFlag() {
		t.Fatal("the viewer took the lease from a live holder")
	}

	a.ClosePeers() // a clean close releases, so the survivor needs no TTL
	b.leaseTick()
	if !b.writerFlag() || b.WriterWord() != writerThis || b.IsViewer() {
		t.Fatalf("the survivor writes after the holder's close: writer=%v word=%q viewer=%v", b.writerFlag(), b.WriterWord(), b.IsViewer())
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

// TestViewerWritesNothing: every road a model turn or a destructive act begins
// refuses on a viewer with the one error, a held -p hands its text back, and
// a background report never turns the model.
func TestViewerWritesNothing(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	openClaimed(t, state, proj) // the writer
	b := openClaimed(t, state, proj)

	sends := []struct {
		name string
		send func() error
	}{
		{"message", func() error { return b.AgentSendUser("hello") }},
		{"delivered prompt", func() error { _, _, err := b.sendPrompt("hello again"); return err }},
		{"goal", func() error { return b.SetGoal("make check is clean") }},
		{"loop", func() error { return b.SetLoop("1m polish the readme") }},
		{"reset", func() error {
			reset := slices.IndexFunc(b.chatCommands(), func(c *Command) bool { return c.Name == "reset" })
			return b.chatCommands()[reset].GoFn(b, "", false)
		}},
	}
	for _, s := range sends {
		t.Run(s.name, func(t *testing.T) {
			if err := s.send(); !errors.Is(err, errNotWriter) {
				t.Fatalf("got %v, want the read-only refusal", err)
			}
		})
	}

	b.RunShell("echo no") // !command writes user rows into the shared chat
	if n, err := b.DB.LogCount(); err != nil || n != 0 {
		t.Fatalf("the viewer's !command wrote %d log rows (err %v)", n, err)
	}

	b.HoldPrompt("do this now") // a follower booted with -p
	if err := b.SendHeld("do this now"); !errors.Is(err, errNotWriter) {
		t.Fatalf("the held prompt: %v", err)
	}
	if ui, ok := b.hooks().(*fakeUI); ok {
		ui.mu.Lock()
		handBack := slices.Contains(ui.composed, "do this now")
		ui.mu.Unlock()
		if !handBack {
			t.Fatal("the refused hold did not hand its text back to the composer")
		}
	}

	b.deliverReport(b.AgentSessionID(), report{id: "gone-run", agent: "general", status: reportStatus("done"), description: "counting", body: "found 3"}, "")
	if b.AgentActive() {
		t.Fatal("the viewer started a model turn for a report")
	}
}
