package brain

import (
	"errors"
	"fmt"
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
		_, _, err = Apply(p, mem("s1", strings.Repeat("w", 3200)), "", later)
		if _, cut := errors.AsType[UserCutError](err); !cut {
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

func TestPromoteMemory(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	later := at.Add(48 * time.Hour)
	promote := func(t *testing.T, p Partition, id, name, description, readIf string) {
		t.Helper()
		if _, err := Promote(p, "s2", id, name, description, readIf, later); err != nil {
			t.Fatal(err)
		}
	}
	lintClean := func(t *testing.T, p Partition, id string) {
		t.Helper()
		for _, f := range Lint(p) {
			if strings.Contains(f.Text, id) {
				t.Fatalf("lint names %s: %s", id, f.Text)
			}
		}
	}
	// a catalog the promotion's catalog line fits: the partition holds
	// memories.md only, so a.md sorts first whatever the file count
	seedCatalog := func(t *testing.T, p Partition, n int) {
		t.Helper()
		if err := os.MkdirAll(p.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for i := range n {
			f := fmt.Sprintf("f%02d.md", i)
			if err := os.WriteFile(filepath.Join(p.Dir, f), ComposeFile("d", "r", "b"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("a live line moves whole, the trail explains it", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		lg := openLedger(t, p)
		m, _, err := Apply(p, mem("s1", "the dploy gate lives in ci/pipeline"), "", at)
		if err != nil {
			t.Fatal(err)
		}
		composed, err := Promote(p, "s2", m.ID, "dploy.md", "the dploy gate: what it does", "before touching ci/", later)
		if err != nil {
			t.Fatal(err)
		}
		if lines, _, _ := readMemories(p); len(lines) != 0 {
			t.Fatalf("lines = %+v", lines)
		}
		onDisk, err := os.ReadFile(filepath.Join(p.Dir, "dploy.md"))
		if err != nil || string(onDisk) != string(composed) {
			t.Fatalf("file = %q, %v", onDisk, err)
		}
		fm, err := ParseFrontmatter(string(onDisk))
		if err != nil || fm.Description != "the dploy gate: what it does" || fm.ReadIf != "before touching ci/" {
			t.Fatalf("frontmatter = %+v, %v (a value holding \": \" must round-trip)", fm, err)
		}
		line, err := Locate(p, lg, m.ID)
		if err != nil || line.Text != "the dploy gate lives in ci/pipeline" {
			t.Fatalf("locate = (%+v, %v)", line, err)
		}
		ops, _ := lg.ops()
		kinds := make([]OpKind, 0, len(ops))
		for _, op := range ops {
			kinds = append(kinds, op.Op)
		}
		if !slices.Equal(kinds, []OpKind{OpAdd, OpSupersede, OpWrite}) {
			t.Fatalf("op kinds = %v", kinds)
		}
		if ops[1].SupersededBy != "dploy.md" || ops[1].Text != "the dploy gate lives in ci/pipeline" {
			t.Fatalf("supersede = %+v", ops[1])
		}
		lintClean(t, p, m.ID)
	})

	t.Run("a ledger-only id promotes out of the archive", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		lg := openLedger(t, p)
		var archived string
		for i := range 6 {
			m, cut, err := Apply(p, mem("s1", strings.Repeat("w", 500)), "", at.Add(-time.Duration(6-i)*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if len(cut) > 0 {
				archived = cut[0]
			}
			_ = m
		}
		if archived == "" {
			t.Fatal("the fill cut nothing")
		}
		if _, err := Promote(p, "s2", archived, "old.md", "an old fact", "when it matters", later); err != nil {
			t.Fatal(err)
		}
		if lines, _, _ := readMemories(p); slices.ContainsFunc(lines, func(l Line) bool { return l.ID == archived }) {
			t.Fatal("the archived line came back live")
		}
		trail, _ := lg.ByID(archived)
		last := trail[len(trail)-1]
		if last.Op != OpSupersede || last.Text != "" || last.SupersededBy != "old.md" {
			t.Fatalf("supersede = %+v", last)
		}
		lintClean(t, p, archived)
	})

	t.Run("the checks refuse with nothing written", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		lg := openLedger(t, p)
		m, _, _ := Apply(p, mem("s1", "one fact"), "", at)
		forgotten, _, _ := Apply(p, mem("s1", "a forgotten fact"), "", at)
		if err := Forget(p, "s1", forgotten.ID, later); err != nil {
			t.Fatal(err)
		}
		seedCatalog(t, p, MaxCatalogFiles)
		for _, tc := range []struct {
			name, id, name2, description, readIf, want string
		}{
			{name: "unknown id", id: "zzzz9999", name2: "x.md", description: "d", readIf: "r", want: "no such memory"},
			{name: "forgotten", id: forgotten.ID, name2: "x.md", description: "d", readIf: "r", want: "is forgotten; save it fresh instead"},
			{name: "missing frontmatter", id: m.ID, name2: "x.md", description: "d", readIf: "", want: "needs description and read_if"},
			{name: "not a brain file name", id: m.ID, name2: "sub/x.md", description: "d", readIf: "r", want: "bare .md name"},
			{name: "the reserved name", id: m.ID, name2: MemFile, description: "d", readIf: "r", want: "bare .md name"},
			{name: "a taken name", id: m.ID, name2: "f00.md", description: "d", readIf: "r", want: "already exists"},
			{name: "a full catalog", id: m.ID, name2: "zz.md", description: "d", readIf: "r", want: "catalog is full"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if err := CheckPromote(p, tc.id, tc.name2, tc.description, tc.readIf); err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("check = %v, want %q", err, tc.want)
				}
				if _, err := Promote(p, "s2", tc.id, tc.name2, tc.description, tc.readIf, later); err == nil {
					t.Fatal("promote applied past a refused check")
				}
			})
		}
		ops, _ := lg.ops()
		if len(ops) != 3 || !slices.EqualFunc(ops, []OpKind{OpAdd, OpAdd, OpForget}, func(o *Op, k OpKind) bool { return o.Op == k }) {
			t.Fatalf("a refused promote journaled %+v", ops)
		}
		if lines, _, _ := readMemories(p); !slices.ContainsFunc(lines, func(l Line) bool { return l.ID == m.ID }) {
			t.Fatal("a refused promote moved the line")
		}
		lintClean(t, p, m.ID)
	})

	t.Run("already promoted refuses while the file serves, retries once it is gone", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		m, _, _ := Apply(p, mem("s1", "one fact"), "", at)
		promote(t, p, m.ID, "out.md", "d", "r")
		if err := CheckPromote(p, m.ID, "out.md", "d2", "r2"); err == nil || !strings.Contains(err.Error(), "already promoted") {
			t.Fatalf("second promote = %v", err)
		}
		if err := os.Remove(filepath.Join(p.Dir, "out.md")); err != nil {
			t.Fatal(err)
		}
		// the file is gone: a half-done promote retries
		if _, err := Promote(p, "s2", m.ID, "out.md", "d2", "r2", later); err != nil {
			t.Fatal(err)
		}
		lintClean(t, p, m.ID)
	})

	t.Run("forget refuses a promoted id and journals once the file is dropped", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		lg := openLedger(t, p)
		m, _, _ := Apply(p, mem("s1", "one fact"), "", at)
		promote(t, p, m.ID, "out.md", "d", "r")
		if err := Forget(p, "s1", m.ID, later); err == nil || !strings.Contains(err.Error(), "is promoted to out.md; drop the file") {
			t.Fatalf("forget = %v", err)
		}
		if err := os.Remove(filepath.Join(p.Dir, "out.md")); err != nil {
			t.Fatal(err)
		}
		if err := Forget(p, "s1", m.ID, later); err != nil {
			t.Fatal(err)
		}
		if lines, _, _ := readMemories(p); slices.ContainsFunc(lines, func(l Line) bool { return l.ID == m.ID }) {
			t.Fatal("the forgotten line stayed live")
		}
		trail, _ := lg.ByID(m.ID)
		if trail[len(trail)-1].Op != OpForget {
			t.Fatalf("trail = %+v", trail)
		}
		lintClean(t, p, m.ID)
	})

	t.Run("ComposeFile round-trips values a naive composer would break", func(t *testing.T) {
		b := ComposeFile(`uses ": " and "quotes"`, `has \ backslashes`, "body")
		fm, err := ParseFrontmatter(string(b))
		if err != nil || fm.Description != `uses ": " and "quotes"` {
			t.Fatalf("compose = %q, %+v, %v", b, fm, err)
		}
	})
}
