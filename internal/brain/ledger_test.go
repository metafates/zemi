package brain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/store"
)

func TestLedger(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

	t.Run("insert and latest state per id", func(t *testing.T) {
		p := testPartition(t)
		openStore(t, p)
		lg, err := p.OpenLedger()
		if err != nil {
			t.Fatal(err)
		}
		defer lg.Close()

		a := &Op{
			ID: "abcd2345", Op: OpAdd, Text: "one", Hash: textHash("one"),
			Scope: store.ScopeProject, Origin: store.OriginLearned, TS: at.UnixMilli(),
		}
		b := &Op{
			ID: "efgh6789", Op: OpAdd, Text: "two", Hash: textHash("two"),
			Scope: store.ScopeProject, Origin: store.OriginLearned, TS: at.UnixMilli(),
		}
		for _, op := range []*Op{a, b} {
			if err := lg.Insert(op); err != nil {
				t.Fatal(err)
			}
		}
		// an edit makes the prior text trail
		e := &Op{
			ID: "abcd2345", Op: OpEdit, Text: "one, moved", PriorText: "one",
			Hash: textHash("one, moved"), Scope: store.ScopeProject,
			Origin: store.OriginLearned, TS: at.Add(time.Hour).UnixMilli(),
		}
		if err := lg.Insert(e); err != nil {
			t.Fatal(err)
		}
		latest, err := lg.Latest()
		if err != nil {
			t.Fatal(err)
		}
		if len(latest) != 2 {
			t.Fatalf("latest = %+v", latest)
		}
		if latest[0].ID != "abcd2345" || latest[0].Text != "one, moved" || latest[0].PriorText != "one" {
			t.Fatalf("newest = %+v", latest[0])
		}
		// the full trail is oldest first
		trail, err := lg.ByID("abcd2345")
		if err != nil || len(trail) != 2 || trail[0].Text != "one" || trail[1].Op != OpEdit {
			t.Fatalf("trail = %+v, %v", trail, err)
		}
		// the prior bytes of a write op round-trip, truncation flag included
		w := &Op{
			ID: "repo-map.md", Op: OpWrite, Prior: []byte(strings.Repeat("x", ContentCap+1)),
			Truncated: true, Existed: true, Scope: store.ScopeProject, TS: at.UnixMilli(),
		}
		if err := lg.Insert(w); err != nil {
			t.Fatal(err)
		}
		got, err := lg.ByID("repo-map.md")
		if err != nil || len(got) != 1 {
			t.Fatalf("write op trail = %+v, %v", got, err)
		}
		if len(got[0].Prior) != ContentCap+1 || !got[0].Truncated || !got[0].Existed {
			t.Fatalf("prior bytes = %d, truncated = %t, existed = %t", len(got[0].Prior), got[0].Truncated, got[0].Existed)
		}
	})

	t.Run("no store is an empty ledger and a failed write", func(t *testing.T) {
		p := testPartition(t)
		lg, err := p.OpenLedger() // the store was never opened
		if err != nil {
			t.Fatal(err)
		}
		defer lg.Close()
		if trail, err := lg.ByID("abcd2345"); err != nil || trail != nil {
			t.Fatalf("empty ledger read = %+v, %v", trail, err)
		}
		if err := lg.Insert(&Op{ID: "abcd2345", Op: OpAdd}); err == nil {
			t.Fatal("inserted with no store")
		}
	})
}

func TestLedgerFailsClosedOnABrokenStore(t *testing.T) {
	p := testPartition(t)
	if err := os.MkdirAll(filepath.Dir(p.DB), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.DB, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if lg, err := p.OpenLedger(); err == nil {
		lg.Close()
		t.Fatal("opened a store that is not a database")
	}
}
