package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func addMem(t *testing.T, db *DB, scope, kind, text string, tags []string) *Memory {
	t.Helper()
	m := &Memory{Scope: scope, Kind: kind, Text: text, Tags: tags, Origin: "user", ProvenanceSessionID: "s9"}
	if err := db.AddMemory(m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMemoryCRUDAndFTS(t *testing.T) {
	db := openTest(t)
	clicks := addMem(t, db, "project", "fact", "the deploy gate lives in ci/pipeline", []string{"ci", "deploy"})
	rust := addMem(t, db, "user", "preference", "prefers rust over go for tools", nil)
	addMem(t, db, "project", "gotcha", "unrelated note about teas", nil)

	hits, err := db.SearchMemory("project", "deploy gate", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != clicks.ID || hits[0].Score == 0 {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].ProvenanceSessionID != "s9" {
		t.Fatalf("provenance lost: %+v", hits[0])
	}

	// Other-scope matches stay out even when the words match.
	hits, err = db.SearchMemory("user", "deploy gate", 10, true)
	if err != nil || len(hits) != 0 {
		t.Fatalf("scope leak: %+v err=%v", hits, err)
	}

	m, err := db.GetMemory(rust.ID)
	if err != nil || m.Text != rust.Text {
		t.Fatalf("get: %+v err=%v", m, err)
	}

	// bump=true stamps the hit accounting.
	before, _ := db.GetMemory(clicks.ID)
	if _, err := db.SearchMemory("project", "deploy", 10, true); err != nil {
		t.Fatal(err)
	}
	after, _ := db.GetMemory(clicks.ID)
	if after.HitCount != before.HitCount+1 || after.LastHitAt == 0 {
		t.Fatalf("bump missing: before=%+v after=%+v", before, after)
	}
	hits, _ = db.SearchMemory("project", "deploy", 10, false)
	if len(hits) == 0 {
		t.Fatal("match vanished")
	}

	// Empty query: recency order, score 0, no bump.
	hits, err = db.SearchMemory("project", "  ", 10, true)
	if err != nil || len(hits) != 2 {
		t.Fatalf("empty query: %+v err=%v", hits, err)
	}
	if hits[0].Score != 0 {
		t.Fatalf("empty query score = %v", hits[0].Score)
	}
}

func TestMemorySearchFtsSyntaxIsNeutralized(t *testing.T) {
	db := openTest(t)
	addMem(t, db, "project", "fact", "not_neo expression", nil)
	for _, q := range []string{`not "quoted"`, `NEAR(a b)`, `col:val`, `"unclosed`, `*` + "\x00"} {
		if _, err := db.SearchMemory("project", q, 10, false); err != nil {
			t.Fatalf("query %q errored: %v", q, err)
		}
	}
}

func TestMemorySupersedeAndDelete(t *testing.T) {
	db := openTest(t)
	old := addMem(t, db, "project", "fact", "gate is at ci/old", nil)
	new := addMem(t, db, "project", "fact", "gate is at ci/new", nil)

	if err := db.SupersedeMemory(old.ID, new.ID); err != nil {
		t.Fatal(err)
	}
	live, _ := db.SearchMemory("project", "gate", 10, false)
	if len(live) != 1 || live[0].ID != new.ID {
		t.Fatalf("superseded still ranks: %+v", live)
	}
	if m, _ := db.GetMemory(old.ID); m.SupersededBy != new.ID {
		t.Fatalf("superseded_by = %q", m.SupersededBy)
	}
	list, _ := db.ListMemory("project", false)
	if len(list) != 1 {
		t.Fatalf("live list = %+v", list)
	}
	list, _ = db.ListMemory("project", true)
	if len(list) != 2 {
		t.Fatalf("full list = %+v", list)
	}

	if err := db.DeleteMemory(new.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetMemory(new.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := db.GetMemory(old.ID); err != nil {
		t.Fatalf("supersede target deleted: %v", err)
	}
}

func TestMemoryTriggersKeepFTSInSync(t *testing.T) {
	db := openTest(t)
	m := addMem(t, db, "project", "fact", "brew is the package manager", nil)

	// Update text: old words must vanish, new ones must match.
	m.Text = "mise is the package manager"
	if err := db.UpdateMemoryText(m.ID, m.Text, m.Origin); err != nil {
		t.Fatal(err)
	}
	hits, _ := db.SearchMemory("project", "brew", 10, false)
	if len(hits) != 0 {
		t.Fatalf("stale fts row after update: %+v", hits)
	}
	hits, _ = db.SearchMemory("project", "mise", 10, false)
	if len(hits) != 1 {
		t.Fatalf("fts missed updated text: %+v", hits)
	}

	// Delete: the word must vanish from the index.
	if err := db.DeleteMemory(m.ID); err != nil {
		t.Fatal(err)
	}
	hits, _ = db.SearchMemory("project", "mise", 10, false)
	if len(hits) != 0 {
		t.Fatalf("stale fts row after delete: %+v", hits)
	}
}

// TestMemoryFTSRebuiltWhenOutOfSync: an index that lost its rows is rebuilt
// at open, so search does not miss memories the listing still shows.
func TestMemoryFTSRebuiltWhenOutOfSync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zemi.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	addMem(t, db, "project", "fact", "the deploy gate lives in ci/pipeline", nil)
	if _, err := db.db.Exec(`INSERT INTO memory_fts(memory_fts) VALUES('delete-all')`); err != nil {
		t.Fatal(err)
	}
	if hits, _ := db.SearchMemory("project", "deploy", 10, false); len(hits) != 0 {
		t.Fatalf("index was not emptied: %+v", hits)
	}
	db.Close()

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hits, err := db.SearchMemory("project", "deploy", 10, false)
	if err != nil || len(hits) != 1 {
		t.Fatalf("open did not rebuild the index: %+v %v", hits, err)
	}
}

func TestMemoryValidation(t *testing.T) {
	db := openTest(t)
	for _, m := range []*Memory{
		{Kind: "fact", Text: "t", Origin: "user"},
		{Scope: "user", Text: "t", Origin: "user"},
		{Scope: "user", Kind: "fact", Origin: "user"},
		{Scope: "user", Kind: "fact", Text: "t"},
	} {
		if err := db.AddMemory(m); err == nil {
			t.Fatalf("want error for %+v", m)
		}
	}
}
