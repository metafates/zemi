package brain

import (
	"errors"
	"os"
	"path/filepath"
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

func TestMemoriesWritePath(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	later := at.Add(48 * time.Hour)

	t.Run("add prepends, edit refreshes and re-prepends", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		first, err := Add(p, "sess0001", "first fact", "I said so", store.OriginLearned, at)
		if err != nil {
			t.Fatal(err)
		}
		second, err := Add(p, "sess1234", "second fact", "", store.OriginUser, at)
		if err != nil {
			t.Fatal(err)
		}
		edited, err := Edit(p, "sess1234", first.ID, "the dploy gate lives in ci/pipeline", later)
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

	t.Run("supersede and forget remove, the ledger explains", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		lg := openLedger(t, p)
		a, _ := Add(p, "s1", "one", "", store.OriginLearned, at)
		b, _ := Add(p, "s1", "two", "", store.OriginLearned, at)
		if err := Supersede(p, "s1", a.ID, b.ID, later); err != nil {
			t.Fatal(err)
		}
		lines, _, _ := readMemories(p)
		if len(lines) != 1 || lines[0].ID != b.ID {
			t.Fatalf("lines = %+v", lines)
		}
		if err := Forget(p, "s1", b.ID, later); err != nil {
			t.Fatal(err)
		}
		if lines, _, _ := readMemories(p); len(lines) != 0 {
			t.Fatalf("lines = %+v", lines)
		}
		// the removed ids resolve through the ledger trail
		for _, id := range []string{a.ID, b.ID} {
			line, live, err := Locate(p, lg, id)
			if err != nil || live || line.Text == "" {
				t.Fatalf("locate %s = (%+v, %t, %v)", id, line, live, err)
			}
		}
		// and a forget of a cut id writes its explainer without a live line
		if err := Forget(p, "s1", a.ID, later); err != nil {
			t.Fatal(err)
		}
		if err := Forget(p, "s1", "zzzz9999", later); !errors.Is(err, ErrNoMemory) {
			t.Fatalf("unknown id: %v", err)
		}
		// every row the write path inserts carries its D7 kind
		ops, err := lg.ops()
		if err != nil {
			t.Fatal(err)
		}
		kinds := make([]string, 0, len(ops))
		for _, op := range ops {
			kinds = append(kinds, op.Op)
		}
		if !slicesSame(kinds, []string{OpAdd, OpAdd, OpSupersede, OpForget, OpForget}) {
			t.Fatalf("op kinds = %v", kinds)
		}
	})

	t.Run("cut archives learned lines and refuses a user line", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		lg := openLedger(t, p)
		var ids []string
		for i := range 6 {
			m, err := Add(p, "s1", strings.Repeat("w", 500), "", store.OriginLearned, at.Add(-time.Duration(6-i)*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, m.ID)
		}
		cut, err := Cut(p, "s1", 0, later)
		if err != nil || len(cut) == 0 {
			t.Fatalf("cut = %v, %v", cut, err)
		}
		if !slicesSame(cut, ids[:len(cut)]) {
			t.Fatalf("cut = %v, oldest are %v", cut, ids)
		}
		lines, _, _ := readMemories(p)
		used := 0
		for _, l := range lines {
			used += LineSize(l.Text)
		}
		if used > ScopeBudget {
			t.Fatalf("cut left %d bytes over budget", used)
		}
		// a scope holding a user line refuses a cut that would reach it
		if _, err := Add(p, "s1", strings.Repeat("u", 3200), "", store.OriginUser, later); err != nil {
			t.Fatal(err)
		}
		if _, err := Cut(p, "s1", 0, later); !errors.Is(err, ErrUserCut) {
			t.Fatalf("user cut = %v", err)
		}
		// nothing was written by the refused cut
		ops, err := lg.Latest()
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range ops {
			if op.ID == ids[0] && op.Op != OpArchive {
				t.Fatalf("cut row kind = %q", op.Op)
			}
			if op.Op == OpArchive && op.ID == ids[len(ids)-1] {
				t.Fatal("refused cut archived the oldest line")
			}
		}
	})

	t.Run("locate prefers the live line", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		lg := openLedger(t, p)
		m, _ := Add(p, "s1", "live one", "quote", store.OriginLearned, at)
		line, live, err := Locate(p, lg, m.ID)
		if err != nil || !live || line.Text != "live one" {
			t.Fatalf("locate = (%+v, %t, %v)", line, live, err)
		}
	})
}

// slicesSame reports a and b hold the same ids, in order.
func slicesSame(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	if _, err := Add(p, "s1", "first", "", store.OriginLearned, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.memPath()); err != nil {
		t.Fatalf("first write created no file: %v", err)
	}
	// a partition whose store was never opened fails its ledger insert
	empty := PrivatePartition(filepath.Dir(p.Dir), "/nowhere")
	if _, err := Add(empty, "s1", "x", "", store.OriginLearned, time.Now()); err == nil {
		t.Fatal("wrote a ledger entry with no store")
	}
}
