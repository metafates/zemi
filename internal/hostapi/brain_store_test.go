package hostapi

// Tests for the brain's rewind halves and the failure paths the design
// calls out by name (DESIGN §7, §10.3).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/brain"
	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// rewindHost gives a trusted host with a live session, its brain dirs and
// stores open, ready for seedMemory.
func rewindHost(t *testing.T) (*Host, string) {
	t.Helper()
	h, _ := newTestHostDB(t)
	h.Settings.SetQuiet("memory.autonomy", "auto")
	h.SetProjectTrusted(true)
	sid := seedSession(t, h)
	addText(t, h, sid, "user", "rewind me")
	_, private := h.brainParts()
	if err := os.MkdirAll(private.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if db, err := store.Open(private.DB); err == nil {
		t.Cleanup(func() { _ = db.Close() })
	}
	return h, sid
}

// TestRewindBrain pins the ledger SELECT's halves: a lock timeout aborts
// before anything moves, learned adds and the budget's archives leave the
// file with a forget entry each, and a superseded line comes back with its
// age from before the replace.
func TestRewindBrain(t *testing.T) {
	h, sid := rewindHost(t)
	_, private := h.brainParts()
	lg, err := private.OpenLedger()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg.Close() }()
	// a learned line the session wrote, a user line it must not touch, a
	// superseded pair and an archived (budget-cut) line; the writes postdate
	// the first prompt, so the rewind's range holds them
	at := time.Now()
	_, _, err = brain.Apply(private, store.Memory{Text: "learned first", ProvenanceSessionID: sid, Origin: store.OriginLearned}, "", at)
	if err != nil {
		t.Fatal(err)
	}
	// a learned line an earlier session wrote, which this session's writes
	// then cut: the rewind un-archives it
	agedID := "aa9a1b2c"
	aged := &store.Memory{ID: agedID, Scope: store.ScopeProject, Text: "the aged truth", ProvenanceSessionID: "seed", Origin: store.OriginLearned, CreatedAt: at.Add(-time.Hour).UnixMilli(), UpdatedAt: at.Add(-time.Hour).UnixMilli()}
	seedMemory(t, h, aged)
	oldID := "ss1a2b3c"
	seedMemory(t, h, &store.Memory{ID: oldID, Scope: store.ScopeProject, Text: "the old truth", ProvenanceSessionID: "seed", Origin: store.OriginLearned, CreatedAt: at.UnixMilli(), UpdatedAt: at.UnixMilli()})
	fresh, cut, err := brain.Apply(private, store.Memory{ID: "ss4d5e6f", Text: "the new truth", ProvenanceSessionID: sid, Origin: store.OriginLearned}, oldID, at)
	if err != nil || len(cut) != 0 {
		t.Fatalf("apply: %+v %v %v", fresh, cut, err)
	}
	ssID := fresh.ID
	// archives: fill the scope and let the cut take the oldest learned lines
	var archived []string
	for range 6 {
		_, c, err := brain.Apply(private, store.Memory{Text: strings.Repeat("w", 500), ProvenanceSessionID: sid, Origin: store.OriginLearned}, "", at)
		if err != nil {
			t.Fatal(err)
		}
		archived = append(archived, c...)
	}
	if len(archived) == 0 {
		t.Fatal("the fills cut nothing")
	}
	yours, _, err := brain.Apply(private, store.Memory{Text: "yours", ProvenanceSessionID: sid, Origin: store.OriginUser}, "", at)
	if err != nil {
		t.Fatal(err)
	}
	addText(t, h, sid, "user", "after the writes")

	// the locks are held: the rewind aborts with nothing truncated
	unlock, err := brain.Lock(private)
	if err != nil {
		t.Fatal(err)
	}
	_, rerr := h.RewindTo(promptsFirst(t, h))
	unlock()
	if rerr == nil || !errors.Is(rerr, brain.ErrLocked) {
		t.Fatalf("the rewind ran under a held store: %v", rerr)
	}
	if rows, _ := h.DB.Messages(sid); len(rows) == 0 {
		t.Fatal("the abort cut the conversation")
	}

	first := promptsFirst(t, h)
	if _, err := h.RewindTo(first); err != nil {
		t.Fatal(err)
	}
	lines, _ := brain.Lines(private)
	live := map[string]string{}
	for _, l := range lines {
		live[l.ID] = l.Text
	}
	// the learned add is gone, the user line the session added survives both
	// halves, the superseded line is live again with its replacement gone,
	// and the archived line cut from an earlier session came back from the
	// ledger's prior text (DESIGN §7)
	for id, want := range map[string]string{
		fresh.ID: "", yours.ID: "yours", oldID: "the old truth",
		ssID: "", agedID: "the aged truth",
	} {
		if live[id] != want {
			t.Fatalf("%s = %q, want %q", id, live[id], want)
		}
	}
	// every removal wrote its forget entry
	ops, _ := lg.Session(sid, 0, at.Add(time.Minute).UnixMilli())
	kinds := map[string]int{}
	for _, op := range ops {
		kinds[op.Op]++
	}
	if kinds[brain.OpForget] < 2 || kinds[brain.OpSupersede] == 0 || kinds[brain.OpArchive] == 0 {
		t.Fatalf("ledger halves: %+v", kinds)
	}
}

// promptsFirst is the id of the session's first user prompt, what a rewind
// to the very beginning targets.
func promptsFirst(t *testing.T, h *Host) string {
	t.Helper()
	rows, err := h.DB.Messages(h.AgentSessionID())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, ok := userText(r); ok {
			return r.ID
		}
	}
	t.Fatal("no prompt to rewind to")
	return ""
}

// TestRewindBrainWrites restores the session's file-mode writes newest
// first: an overwrite to its prior bytes, a creation deleted, and a
// truncated prior deferring to the checkpoint (DESIGN §7).
func TestRewindBrainWrites(t *testing.T) {
	h, sid := rewindHost(t)
	_, private := h.brainParts()
	path := filepath.Join(private.Dir, "map.md")
	fm := "---\ndescription: d\nread_if: r\n---\n"
	if _, err := h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Path: path, Content: fm + "one\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Path: path, Content: fm + "two\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Path: filepath.Join(private.Dir, "new.md"), Content: fm + "born here\n"}); err != nil {
		t.Fatal(err)
	}
	addText(t, h, sid, "user", "after the writes")
	if _, err := h.RewindTo(promptsFirst(t, h)); err != nil {
		t.Fatal(err)
	}
	// the session created map.md in the range and new.md too: the oldest
	// entries' existed=false deletes both (DESIGN §7)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("map.md survived the rewind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(private.Dir, "new.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file the session created survived: %v", err)
	}
	// the write set cleared: the next write is a creation that applies
	msg, err := h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Path: path, Content: fm + "three\n"})
	if err != nil || !strings.Contains(msg, "wrote map.md") {
		t.Fatalf("the write set did not clear: %q, %v", msg, err)
	}
}

// TestBrainCrashOrdering: a crash between the ledger insert and the file
// replace leaves the lint finding, never a provenance-less live line (DESIGN §7).
func TestBrainCrashOrdering(t *testing.T) {
	h, sid := rewindHost(t)
	_, private := h.brainParts()
	lg, err := private.OpenLedger()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg.Close() }()
	// the add entry inserted, the file replace never happened
	if err := lg.Insert(&brain.Op{
		Op: brain.OpAdd, ID: "crash1ab", Text: "from a crash", SessionID: sid,
		Hash: brain.FileHash([]byte("from a crash")), Scope: store.ScopeProject,
		Origin: store.OriginLearned, TS: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	findings := brain.Lint(private)
	if !slicesContainsFindings(findings, "crash1ab", "gone from memories.md") {
		t.Fatalf("the divergence is not named: %+v", findings)
	}
	// and nothing injected: the store's live lines hold no such text
	live, _ := h.memoryList(false)
	for _, m := range live {
		if m.Text == "from a crash" {
			t.Fatal("a provenance-less live line injected")
		}
	}
}

// TestMoveOutExplainsAbsence: after a cross-scope move no lint finding
// claims divergence (DESIGN §7).
func TestMoveOutExplainsAbsence(t *testing.T) {
	h, _ := rewindHost(t)
	if err := h.Remember("deploy by hand"); err != nil {
		t.Fatal(err)
	}
	m := projectMemories(t, h)[0]
	if err := h.moveMemory(m.ID, m.Scope, otherScope(m.Scope)); err != nil {
		t.Fatal(err)
	}
	_, private := h.brainParts()
	for _, f := range brain.Lint(private) {
		if strings.Contains(f.Text, "gone from") || strings.Contains(f.Text, "outside the ledger") {
			t.Fatalf("the move-out left a divergence finding: %+v", f)
		}
	}
}

// TestCrossScopeSupersede: a replaces whose memory moved to the other
// partition supersedes it there and lands the new line in its own (DESIGN §7) -
// what the store swap strands a pre-swap proposal into.
func TestCrossScopeSupersede(t *testing.T) {
	h, sid := rewindHost(t)
	user, private := h.brainParts()
	for _, p := range []brain.Partition{user, private} {
		if err := os.MkdirAll(p.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old, _, err := brain.Apply(user, store.Memory{ID: "uu7a1b2c", Text: "the user truth", ProvenanceSessionID: sid, Origin: store.OriginLearned}, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	res := &learn.Result{
		Memory:     store.Memory{Scope: store.ScopeProject, Text: "a different project fact", ProvenanceSessionID: sid, Origin: store.OriginLearned},
		Supersedes: old.ID,
	}
	if _, err := h.storeProposal(res); err != nil {
		t.Fatal(err)
	}
	if lines, _ := brain.Lines(user); len(lines) != 0 {
		t.Fatalf("the replaced line stayed: %+v", lines)
	}
	live, _ := h.memoryList(false)
	if len(live) != 1 || live[0].Scope != store.ScopeProject || live[0].Text != "a different project fact" {
		t.Fatalf("live = %+v", live)
	}
	if by := supersededBy(t, h, old.ID); by != live[0].ID {
		t.Fatalf("superseded by %q, want %q", by, live[0].ID)
	}
}

// TestRewindMoveOut: rewinding a turn that moved a learned line across
// scopes restores the source's line and drops the destination copy
// (DESIGN §7), not a deletion from both.
func TestRewindMoveOut(t *testing.T) {
	h, sid := rewindHost(t)
	user, private := h.brainParts()
	for _, p := range []brain.Partition{user, private} {
		if err := os.MkdirAll(p.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	movedID := "aa7a1b2c"
	seedMemory(t, h, &store.Memory{ID: movedID, Scope: store.ScopeProject, Text: "the traveling fact", ProvenanceSessionID: "seed", Origin: store.OriginLearned, UpdatedAt: time.Now().Add(-time.Hour).UnixMilli()})
	if err := h.moveMemory(movedID, store.ScopeProject, store.ScopeUser); err != nil {
		t.Fatal(err)
	}
	addText(t, h, sid, "user", "after the move")
	if _, err := h.RewindTo(promptsFirst(t, h)); err != nil {
		t.Fatal(err)
	}
	lines, _ := brain.Lines(private)
	if len(lines) != 1 || lines[0].ID != movedID || lines[0].Text != "the traveling fact" {
		t.Fatalf("the source line did not come back: %+v", lines)
	}
	if ulines, _ := brain.Lines(user); len(ulines) != 0 {
		t.Fatalf("the destination copy stayed: %+v", ulines)
	}
}

// TestRewindLedgerOnlySupersede: a supersede whose id only the ledger holds
// (a budget cut got there first) restores nothing - the pre-session state
// already has the id archived - and writes no unparseable line.
func TestRewindLedgerOnlySupersede(t *testing.T) {
	h, sid := rewindHost(t)
	_, private := h.brainParts()
	lg, err := private.OpenLedger()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg.Close() }()
	old := time.Now().Add(-time.Hour)
	id := "aa9a1b2c"
	for _, op := range []*brain.Op{
		{Op: brain.OpAdd, ID: id, Text: "the aged truth", SessionID: "seed", Hash: brain.FileHash([]byte("the aged truth")), Scope: store.ScopeProject, Origin: store.OriginLearned, TS: old.UnixMilli()},
		{Op: brain.OpArchive, ID: id, Text: "the aged truth", SessionID: "seed", Hash: brain.FileHash([]byte("the aged truth")), Scope: store.ScopeProject, Origin: store.OriginLearned, TS: old.UnixMilli()},
	} {
		if err := lg.Insert(op); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := brain.Apply(private, store.Memory{Text: "the new truth", ProvenanceSessionID: sid, Origin: store.OriginLearned}, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	addText(t, h, sid, "user", "after the writes")
	if _, err := h.RewindTo(promptsFirst(t, h)); err != nil {
		t.Fatal(err)
	}
	lines, _ := brain.Lines(private)
	for _, l := range lines {
		if l.ID == id || l.Text == "" || l.Origin == "" {
			t.Fatalf("the empty un-supersede wrote a line: %+v", lines)
		}
	}
	raw, err := os.ReadFile(filepath.Join(private.Dir, brain.MemFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, bad := brain.ParseMemories(raw); len(bad) > 0 {
		t.Fatalf("an unparseable line survived: %q", raw)
	}
}

// TestReflectorQueuesFailedApply: an apply the store refuses queues as a
// proposal whose accept applies it, and the delta still marks seen (DESIGN §10.3).
func TestReflectorQueuesFailedApply(t *testing.T) {
	h, sid := rewindHost(t)
	_, private := h.brainParts()
	// a store another zemi holds: the batch queues whole, seen included
	unlock, err := brain.Lock(private)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	m := reflectStub(t, h, `{"memories":[{"scope":"project","text":"the deploy script needs the labels sequence bumped first"}]}`)
	if _, err := h.reflectSession(t.Context(), sid); err != nil {
		t.Fatal(err)
	}
	if m.calls.Load() != 1 {
		t.Fatalf("calls = %d", m.calls.Load())
	}
	if n := len(h.memoryProposals()); n != 1 {
		t.Fatalf("the failed apply did not queue: %d proposals", n)
	}
	if _, seen, _, _ := h.reflectDelta(sid); seen == 0 {
		t.Fatal("the delta was not marked seen")
	}
	live, _ := h.memoryList(false)
	for _, x := range live {
		if x.Text == "the deploy script needs the labels sequence bumped first" {
			t.Fatal("the write landed under a held store")
		}
	}
}

func slicesContainsFindings(fs []brain.Finding, id, text string) bool {
	for _, f := range fs {
		if strings.Contains(f.Text, id) && strings.Contains(f.Text, text) {
			return true
		}
	}
	return false
}
