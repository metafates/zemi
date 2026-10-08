package brain

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/store"
)

func TestMemoriesRoundTrip(t *testing.T) {
	lines, bad := ParseMemories([]byte(
		"- [abcd2345 learned 2026-10-05] the dploy gate lives in ci/pipeline\n" +
			"\n" +
			"- [efgh6789 user 2026-10-01] prefers tabs\n" +
			"not a memory line\n"))
	if len(lines) != 2 || len(bad) != 1 || bad[0].N != 4 {
		t.Fatalf("lines = %+v, bad = %+v", lines, bad)
	}
	if lines[0].ID != "abcd2345" || lines[0].Origin != store.OriginLearned || lines[0].Text != "the dploy gate lives in ci/pipeline" {
		t.Fatalf("line = %+v", lines[0])
	}
	if lines[0].Date.Format(time.DateOnly) != "2026-10-05" {
		t.Fatalf("date = %s", lines[0].Date)
	}
	if got := serialize(lines); string(got) != "- [abcd2345 learned 2026-10-05] the dploy gate lives in ci/pipeline\n- [efgh6789 user 2026-10-01] prefers tabs\n" {
		t.Fatalf("serialize = %q", got)
	}
	for _, ln := range []string{
		"- [abcd2345 learned 2026-10-05]text",      // no space after ]
		"- [abcd2345 learned not-a-date] t",        // date malformed
		"- [short learned 2026-10-05] t",           // id not an id
		"- [abcd2345 robot 2026-10-05] t",          // origin not an origin
		"- abcd2345 learned 2026-10-05] t",         // no [
		"abcd2345 learned 2026-10-05",              // nothing at all
		"- [abcd2345 learned 2026-10-05 no closer", // the ] is missing
	} {
		if _, bad := ParseMemories([]byte(ln)); len(bad) != 1 {
			t.Fatalf("%q parsed as a live line", ln)
		}
	}
}

// mem is a learned one-liner with provenance, the way every writer hands
// its memory to Apply.
func mem(sid, text string) store.Memory {
	return store.Memory{Text: text, ProvenanceSessionID: sid, Origin: store.OriginLearned}
}

func TestMemoriesWritePath(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	later := at.Add(48 * time.Hour)

	t.Run("apply prepends and an edit refreshes and re-prepends", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		first, _, err := Apply(p, mem("sess0001", "first fact"), "", at)
		if err != nil {
			t.Fatal(err)
		}
		second, _, err := Apply(p, store.Memory{Text: "second fact", ProvenanceSessionID: "sess1234", Origin: store.OriginUser}, "", at)
		if err != nil {
			t.Fatal(err)
		}
		if second.Origin != store.OriginUser {
			t.Fatal("origin lost")
		}
		edited, err := Edit(p, "sess1234", first.ID, "the dploy gate lives in ci/pipeline", store.OriginLearned, later)
		if err != nil {
			t.Fatal(err)
		}
		lines, _, err := readMemories(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(lines) != 2 || lines[0].ID != first.ID || lines[1].ID != second.ID {
			t.Fatalf("order = %+v", lines)
		}
		if lines[0].Date.Format(time.DateOnly) != "2026-10-07" || lines[0].Text != edited.Text {
			t.Fatalf("edit did not refresh the date: %+v", lines[0])
		}
	})

	t.Run("replaces supersedes and forget removes, the ledger explains", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		lg := openLedger(t, p)
		a, _, _ := Apply(p, mem("s1", "one"), "", at)
		b, _, _ := Apply(p, mem("s1", "two"), "", at)
		c, cut, err := Apply(p, mem("s1", "three"), a.ID, later)
		if err != nil || len(cut) != 0 {
			t.Fatalf("apply = (%+v, %v, %v)", c, cut, err)
		}
		lines, _, _ := readMemories(p)
		if len(lines) != 2 || lines[0].ID != c.ID || lines[1].ID != b.ID {
			t.Fatalf("lines = %+v", lines)
		}
		if err := Forget(p, "s1", c.ID, later); err != nil {
			t.Fatal(err)
		}
		if err := Forget(p, "s1", b.ID, later); err != nil {
			t.Fatal(err)
		}
		if lines, _, _ := readMemories(p); len(lines) != 0 {
			t.Fatalf("lines = %+v", lines)
		}
		// the file holds none of them, so these resolve through the ledger trail
		for _, id := range []string{a.ID, b.ID, c.ID} {
			line, err := Locate(p, lg, id)
			if err != nil || line.Text == "" {
				t.Fatalf("locate %s = (%+v, %v)", id, line, err)
			}
		}
		// a forget of an id nothing holds is refused
		if err := Forget(p, "s1", "zzzz9999", later); !errors.Is(err, ErrNoMemory) {
			t.Fatalf("unknown id: %v", err)
		}
		// every row the write path inserts carries its kind (DESIGN §7)
		ops, err := lg.ops()
		if err != nil {
			t.Fatal(err)
		}
		kinds := make([]OpKind, 0, len(ops))
		for _, op := range ops {
			kinds = append(kinds, op.Op)
		}
		if !slices.Equal(kinds, []OpKind{OpAdd, OpAdd, OpSupersede, OpAdd, OpForget, OpForget}) {
			t.Fatalf("op kinds = %v", kinds)
		}
	})

	t.Run("the budget cut archives learned lines and names them", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		lg := openLedger(t, p)
		var ids []string
		var cut []string
		for i := range 6 {
			m, c, err := Apply(p, mem("s1", strings.Repeat("w", 500)), "", at.Add(-time.Duration(6-i)*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, m.ID)
			cut = append(cut, c...)
		}
		if !slices.Equal(cut, ids[:1]) {
			t.Fatalf("cut = %v, oldest is %v", cut, ids)
		}
		lines, _, _ := readMemories(p)
		used := 0
		for _, l := range lines {
			used += LineSize(l.Text)
		}
		if used > ScopeBudget {
			t.Fatalf("cut left %d bytes over budget", used)
		}
		ops, err := lg.ops()
		if err != nil {
			t.Fatal(err)
		}
		var archived []string
		for _, op := range ops {
			if op.Op == OpArchive {
				archived = append(archived, op.ID)
			}
		}
		if !slices.Equal(archived, cut) {
			t.Fatalf("archive rows = %v, cut = %v", archived, cut)
		}
	})

	t.Run("an overflow that would cut a user line writes nothing", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		lg := openLedger(t, p)
		yours, _, err := Apply(p, store.Memory{Text: strings.Repeat("u", 300), ProvenanceSessionID: "s1", Origin: store.OriginUser}, "", at)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := Apply(p, mem("s1", strings.Repeat("w", 3200)), "", later); !errors.As(err, &UserCutError{}) {
			t.Fatalf("user cut = %v", err)
		}
		lines, _, _ := readMemories(p)
		if len(lines) != 1 || lines[0].ID != yours.ID {
			t.Fatalf("the refused write landed: %+v", lines)
		}
		ops, err := lg.ops()
		if err != nil {
			t.Fatal(err)
		}
		if len(ops) != 1 || ops[0].Op != OpAdd {
			t.Fatalf("ops = %+v", ops)
		}
	})

	t.Run("restore re-prepends and drops with forget entries", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		lg := openLedger(t, p)
		a, _, _ := Apply(p, mem("s1", "one"), "", at)
		b, _, _ := Apply(p, mem("s1", "two"), "", at)
		if err := Forget(p, "s1", a.ID, later); err != nil {
			t.Fatal(err)
		}
		back := Line{ID: a.ID, Origin: store.OriginLearned, Date: Day(at), Text: "one"}
		if err := Restore(p, "s1", []Line{back}, []string{b.ID}, later); err != nil {
			t.Fatal(err)
		}
		lines, _, _ := readMemories(p)
		if len(lines) != 1 || lines[0] != back {
			t.Fatalf("lines = %+v", lines)
		}
		ops, err := lg.ops()
		if err != nil {
			t.Fatal(err)
		}
		kinds := make([]OpKind, 0, len(ops))
		for _, op := range ops {
			kinds = append(kinds, op.Op)
		}
		if !slices.Equal(kinds, []OpKind{OpAdd, OpAdd, OpForget, OpForget}) {
			t.Fatalf("op kinds = %v", kinds)
		}
	})

	t.Run("locate returns the line the file holds", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		lg := openLedger(t, p)
		m, _, _ := Apply(p, store.Memory{Text: "live one", Quote: "quote", ProvenanceSessionID: "s1", Origin: store.OriginLearned}, "", at)
		lines, _, _ := readMemories(p)
		line, err := Locate(p, lg, m.ID)
		if err != nil || len(lines) != 1 || line != lines[0] {
			t.Fatalf("locate = (%+v, %v), the file holds %+v", line, err, lines)
		}
	})
}

// openStore opens the partition's own store, the way a running zemi has it
// open before any brain work runs.
func openStore(t *testing.T, p Partition) *store.DB {
	t.Helper()
	db, err := store.Open(p.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// openLedger opens the partition's ledger over a store already open.
func openLedger(t *testing.T, p Partition) *Ledger {
	t.Helper()
	lg, err := p.OpenLedger()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })
	return lg
}

func TestMemoriesWriteCreatesThePartition(t *testing.T) {
	p := testPartition(t)
	openStore(t, p)
	if _, _, err := Apply(p, mem("s1", "first"), "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.memPath()); err != nil {
		t.Fatalf("first write created no file: %v", err)
	}
	// a partition whose store was never opened fails its ledger insert
	empty := PrivatePartition(filepath.Dir(p.Dir), "/nowhere")
	if _, _, err := Apply(empty, mem("s1", "x"), "", time.Now()); err == nil {
		t.Fatal("wrote a ledger entry with no store")
	}
}
