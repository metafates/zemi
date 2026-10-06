package brain

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/store"
)

func TestReconcile(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	live := func(id, text string, updated int64) *store.Memory {
		return &store.Memory{
			ID: id, Scope: "project", Text: text, ProvenanceSessionID: "seed",
			Origin: store.OriginLearned, CreatedAt: updated, UpdatedAt: updated,
		}
	}
	// seedRow inserts a frozen-table row the way the pre-brain store's write
	// path did - the table's only writer now (section 7).
	seedRow := func(t *testing.T, p Partition, m *store.Memory) {
		t.Helper()
		db, err := sql.Open("sqlite", p.DB)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`INSERT INTO memory
			(id, scope, text, quote, provenance_session_id, created_at, updated_at, superseded_by, origin)
			VALUES (?, ?, ?, '', ?, ?, ?, ?, ?)`,
			m.ID, m.Scope, m.Text, m.ProvenanceSessionID, m.CreatedAt, m.UpdatedAt, m.SupersededBy, m.Origin); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("imports live rows, keeps superseded ones and re-runs idempotently", func(t *testing.T) {
		p := testPartition(t)
		db := openStore(t, p)
		lg := openLedger(t, p)
		old := &store.Memory{
			ID: "superseded1", Scope: "project", Text: "old truth", ProvenanceSessionID: "seed",
			Origin: store.OriginLearned, UpdatedAt: 1, SupersededBy: "newerid1",
		}
		rows := []*store.Memory{
			live("bbbb2222", "newer fact", 2_000_000_000),
			live("aaaa1111", "older fact", 1_000_000_000),
			old,
		}
		for _, m := range rows {
			seedRow(t, p, m)
		}
		if err := Reconcile(p, rows, at); err != nil {
			t.Fatal(err)
		}
		lines, _, err := readMemories(p)
		if err != nil {
			t.Fatal(err)
		}
		// newest last-write first, then id
		if len(lines) != 2 || lines[0].ID != "bbbb2222" || lines[1].ID != "aaaa1111" {
			t.Fatalf("lines = %+v", lines)
		}
		if lines[0].Date.Format(time.DateOnly) != "1970-01-24" {
			t.Fatalf("the age did not come from updated_at: %+v", lines[0])
		}
		ops, err := lg.ops()
		if err != nil {
			t.Fatal(err)
		}
		if len(ops) != 2 {
			t.Fatalf("ops = %+v", ops)
		}
		for i, op := range ops {
			if op.Op != OpImport || op.Quote != "" || op.SessionID != "seed" || op.Origin != store.OriginLearned {
				t.Fatalf("import %d = %+v", i, op)
			}
		}
		// a re-run changes nothing, and the superseded row still resolves
		if err := Reconcile(p, rows, at); err != nil {
			t.Fatal(err)
		}
		if n := func() int { ops, _ := lg.ops(); return len(ops) }(); n != 2 {
			t.Fatalf("re-run inserted rows: %d", n)
		}
		if m, err := db.GetMemory("superseded1"); err != nil || m.SupersededBy != "newerid1" || m.Text != "old truth" {
			t.Fatalf("the frozen table lost the superseded row: %+v, %v", m, err)
		}
		if _, _, err := Locate(p, lg, "superseded1"); !errors.Is(err, ErrNoMemory) {
			t.Fatalf("a superseded row composed after all: %v", err)
		}
	})

	t.Run("a hand-deleted line never resurrects", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		rows := []*store.Memory{live("aaaa1111", "the fact", 1000)}
		seedRow(t, p, rows[0])
		if err := Reconcile(p, rows, at); err != nil {
			t.Fatal(err)
		}
		// the user's terminal deletes the line; no forget entry is written
		if err := os.WriteFile(p.memPath(), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Reconcile(p, rows, at); err != nil {
			t.Fatal(err)
		}
		lines, _, _ := readMemories(p)
		if len(lines) != 0 {
			t.Fatalf("the hand-deleted line came back: %+v", lines)
		}
	})

	t.Run("a hand-written malformed line survives the import", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		rows := []*store.Memory{live("aaaa1111", "the fact", 1000)}
		seedRow(t, p, rows[0])
		if err := os.MkdirAll(p.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.memPath(), []byte("the user's own stray line\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Reconcile(p, rows, at); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(p.memPath())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(raw, []byte("the user's own stray line")) {
			t.Fatalf("the import deleted a line it did not write: %q", raw)
		}
		lines, _, _ := readMemories(p)
		if len(lines) != 1 || lines[0].ID != "aaaa1111" {
			t.Fatalf("lines = %+v", lines)
		}
	})

	t.Run("the tombstone follows the bytes it stamps", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		rows := []*store.Memory{live("aaaa1111", "the fact", 1000)}
		seedRow(t, p, rows[0])
		lg := openLedger(t, p)
		// the file replace fails: the import entry must not land either, so
		// a re-run still composes
		if err := os.MkdirAll(p.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		lock, err := os.OpenFile(filepath.Join(p.Dir, "memories.lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_ = lock.Close()
		if err := os.Chmod(p.Dir, 0o500); err != nil {
			t.Fatal(err)
		}
		if err := Reconcile(p, rows, at); err == nil {
			t.Fatal("the write did not fail")
		}
		if err := os.Chmod(p.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		ops, err := lg.ops()
		if err != nil {
			t.Fatal(err)
		}
		if len(ops) != 0 {
			t.Fatalf("entries committed before the bytes: %+v", ops)
		}
		// the re-run composes, then the outstanding entries land
		if err := Reconcile(p, rows, at); err != nil {
			t.Fatal(err)
		}
		if ops, _ = lg.ops(); len(ops) != 1 || ops[0].Op != OpImport {
			t.Fatalf("re-run entries: %+v", ops)
		}
	})
}
