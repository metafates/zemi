package brain

import (
	"errors"
	"os"
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
		for _, m := range rows[:2] {
			if err := db.AddMemory(m); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.AddMemory(old); err != nil {
			t.Fatal(err)
		}
		if err := db.SupersedeMemory("superseded1", "newerid1"); err != nil {
			t.Fatal(err)
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
		db := openStore(t, p)
		rows := []*store.Memory{live("aaaa1111", "the fact", 1000)}
		if err := db.AddMemory(rows[0]); err != nil {
			t.Fatal(err)
		}
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
}
