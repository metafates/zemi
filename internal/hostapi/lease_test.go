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
	h.beat() // the heartbeat is what a pushing writer dials
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

	b.leaseTick(false) // a live holder's beat is fresh: the viewer leaves it alone
	if b.writerFlag() {
		t.Fatal("the viewer took the lease from a live holder")
	}

	a.ClosePeers() // a clean close releases, so the survivor needs no TTL
	b.leaseTick(false)
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

	b.leaseTick(false)
	if !b.writerFlag() || b.WriterWord() != writerThis {
		t.Fatalf("the survivor took over the stale lease: writer=%v word=%q", b.writerFlag(), b.WriterWord())
	}

	a.leaseTick(false)
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
	a.leaseTick(false)
	if !a.writerFlag() || a.WriterWord() != writerThis {
		t.Fatalf("an unreachable store demoted the holder: writer=%v word=%q", a.writerFlag(), a.WriterWord())
	}
}

// TestViewerWritesNothing: every road a model turn or a destructive act begins
// refuses on a viewer with the one error, a held -p hands its text back, and
// a background report never turns the model. The typed message is the one
// road that does not refuse: it parks, asks the writer for the chat, and the
// write arrives - the parked queue sends.
func TestViewerWritesNothing(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a := openClaimed(t, state, proj) // the writer
	b := openClaimed(t, state, proj)
	replyStub(t, b, "ok")

	sends := []struct {
		name string
		send func() error
	}{
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

	b.RunShell("echo no") // RunShell, the shell-row writer, refuses on a viewer
	if n, err := b.DB.LogCount(); err != nil || n != 0 {
		t.Fatalf("the viewer's shell command wrote %d log rows (err %v)", n, err)
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

	// the typed message parks and asks; the writer hands the chat over, and
	// the parked queue sends once this window holds the write
	if err := b.AgentSendUser("hello"); err != nil {
		t.Fatalf("the viewer's message did not park: %v", err)
	}
	if _, _, err := b.sendPrompt("hello again"); err != nil {
		t.Fatalf("the delivered message did not park: %v", err)
	}
	for i := 0; i < 500 && b.IsViewer(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if b.IsViewer() || !a.IsViewer() {
		t.Fatalf("the ask did not hand the write over: the asker views=%v the writer views=%v", b.IsViewer(), a.IsViewer())
	}
	for range 500 {
		if n, err := b.DB.LogCount(); err == nil && n >= 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the parked queue never sent")
}

// TestYieldEndsTheTurnAtItsBoundary: an ask that lands mid-turn parks, the
// turn's end hands the lease to the asker, and the demoted writer keeps
// neither the ask nor the write.
func TestYieldEndsTheTurnAtItsBoundary(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a := openClaimed(t, state, proj)
	b := openClaimed(t, state, proj)

	ag := a.agents
	ag.mu.Lock()
	ag.beginTurn() // a turn runs: the ask parks rather than moving the write
	scope := a.claim()
	ag.mu.Unlock()
	a.yieldWrite(b.origin, b.sockPath)
	if !a.yieldPending() {
		t.Fatal("the ask did not park for the running turn")
	}
	if l, err := a.DB.ReadLeader(); err != nil || l.Origin != a.origin {
		t.Fatalf("the parked ask moved the lease early: %+v (%v)", l, err)
	}

	a.nextTurn(scope, nil) // the turn ends at its boundary
	if l, err := b.DB.ReadLeader(); err != nil || l.Origin != b.origin {
		t.Fatalf("the lease did not move to the asker: %+v (%v)", l, err)
	}
	if a.writerFlag() || a.WriterWord() != writerOther || a.yieldPending() {
		t.Fatalf("the writer did not demote: writer=%v word=%q", a.writerFlag(), a.WriterWord())
	}
	b.leaseTick(true)
	if !b.writerFlag() {
		t.Fatal("the asker did not promote")
	}
}

// TestViewerDrawsTheWritersStreamLive: the writer's flushed stream reaches the
// watching window over its socket - the same snapshot it draws itself, clocks
// running between frames - and the viewer owns no turn underneath: its sync
// tick still runs and its composer still refuses.
func TestViewerDrawsTheWritersStreamLive(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a := openClaimed(t, state, proj)
	b := openClaimed(t, state, proj)

	a.SeedStream(StreamSnapshot{Active: true, Text: "mid-reply", Task: "write the docs", Elapsed: time.Second})
	a.pokeViewers()
	var got StreamSnapshot
	for range 500 {
		if got = b.AgentStream(); got.Text == "mid-reply" && got.Active {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.Text != "mid-reply" || !got.Active {
		t.Fatalf("the viewer never drew the pushed stream: %+v", got)
	}
	if b.AgentActive() {
		t.Fatal("a pushed turn made the viewer own one: the tick and the composer would follow")
	}
	if got.Elapsed <= time.Second {
		t.Fatalf("the pushed clock did not run: %v", got.Elapsed)
	}
	if got.Task != "write the docs" {
		t.Fatalf("the pushed task did not arrive: %q", got.Task)
	}
	time.Sleep(25 * time.Millisecond)
	if after := b.AgentStream().Elapsed; after <= got.Elapsed {
		t.Fatalf("the watched clock froze between frames: %v then %v", got.Elapsed, after)
	}

	// the turn's end clears the live view the way the writer's own window sees it
	a.SeedStream(StreamSnapshot{})
	a.pokeViewers()
	for range 500 {
		if got = b.AgentStream(); !got.Active {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.Active || got.Text != "" {
		t.Fatalf("the turn's end never reached the viewer: %+v", got)
	}
}

// TestPushedRowsReachTheViewerWithoutTheTick: a row the writer logs rides the
// same poke as its stream, so the watching window splices it when the frame
// arrives - no tick's second of latency.
func TestPushedRowsReachTheViewerWithoutTheTick(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a := openClaimed(t, state, proj)
	b := openClaimed(t, state, proj)

	if _, err := a.chat.Notice("the loop ran", "run 3 of 5"); err != nil {
		t.Fatalf("Notice: %v", err)
	}
	for range 500 {
		for _, m := range b.Session().Transcript {
			if m.Text == "the loop ran" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the logged row never reached the viewer without its tick")
}

// TestTakeoverDropsTheWatch: the survivor that takes the write stops drawing
// the gone writer's stream - its own snapshot is the truth again.
func TestTakeoverDropsTheWatch(t *testing.T) {
	proj, state := t.TempDir(), t.TempDir()
	a := openClaimed(t, state, proj)
	b := openClaimed(t, state, proj)

	a.SeedStream(StreamSnapshot{Active: true, Text: "frozen mid-reply"})
	a.pokeViewers()
	for range 500 {
		if b.AgentStream().Text == "frozen mid-reply" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if b.AgentStream().Text != "frozen mid-reply" {
		t.Fatal("the viewer never drew the pushed stream")
	}

	a.ClosePeers()     // a clean close releases the lease
	b.leaseTick(false) // the survivor takes the write
	if !b.writerFlag() {
		t.Fatal("the survivor never took the write")
	}
	if s := b.AgentStream(); s.Active || s.Text != "" {
		t.Fatalf("the takeover still draws the gone writer's stream: %+v", s)
	}
}
