package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func addMem(t *testing.T, db *DB, scope, text string) *Memory {
	t.Helper()
	m := &Memory{Scope: scope, Text: text, Quote: "q", Origin: "user", ProvenanceSessionID: "s9"}
	if err := db.AddMemory(m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMemoryCRUD(t *testing.T) {
	db := openTest(t)
	gate := addMem(t, db, "project", "the deploy gate lives in ci/pipeline")
	rust := addMem(t, db, "user", "prefers rust over go for tools")

	m, err := db.GetMemory(rust.ID)
	if err != nil || m.Text != rust.Text || m.Quote != "q" || m.ProvenanceSessionID != "s9" {
		t.Fatalf("get: %+v err=%v", m, err)
	}
	if list, _ := db.ListMemory("project", false); len(list) != 1 || list[0].ID != gate.ID {
		t.Fatalf("scope leak: %+v", list)
	}
	if err := db.UpdateMemoryText(gate.ID, "moved", "learned"); err != nil {
		t.Fatal(err)
	}
	if m, _ := db.GetMemory(gate.ID); m.Text != "moved" || m.Origin != "learned" {
		t.Fatalf("update: %+v", m)
	}
}

func TestMemorySupersedeAndDelete(t *testing.T) {
	db := openTest(t)
	old := addMem(t, db, "project", "gate is at ci/old")
	new := addMem(t, db, "project", "gate is at ci/new")

	if err := db.SupersedeMemory(old.ID, new.ID); err != nil {
		t.Fatal(err)
	}
	if m, _ := db.GetMemory(old.ID); m.SupersededBy != new.ID {
		t.Fatalf("superseded_by = %q", m.SupersededBy)
	}
	list, _ := db.ListMemory("project", false)
	if len(list) != 1 || list[0].ID != new.ID {
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

// oldMemoryShape turns a fresh memory table into the one before schema
// version 2: kind, tags, hit counts and an FTS index.
var oldMemoryShape = []string{
	`ALTER TABLE memory DROP COLUMN quote`,
	`ALTER TABLE memory ADD COLUMN kind TEXT NOT NULL DEFAULT 'fact'`,
	`ALTER TABLE memory ADD COLUMN tags TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE memory ADD COLUMN hit_count INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE memory ADD COLUMN last_hit_at INTEGER NOT NULL DEFAULT 0`,
	`CREATE INDEX memory_scope_hit ON memory(scope, last_hit_at)`,
	`CREATE VIRTUAL TABLE memory_fts USING fts5(text, content='memory', content_rowid='rowid')`,
	`CREATE TRIGGER memory_ai AFTER INSERT ON memory BEGIN INSERT INTO memory_fts(rowid, text) VALUES (new.rowid, new.text); END`,
}

// TestMigrateMemoryColumns: a store with the old memory shape opens, keeps
// its text and gains quote.
func TestMigrateMemoryColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zemi.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range append(oldMemoryShape,
		`INSERT INTO memory (id, scope, kind, text, tags, created_at, updated_at, origin) VALUES ('m1', 'project', 'gotcha', 'old text', 'a,b', 1, 1, 'user')`,
		`PRAGMA user_version = 1`) {
		if _, err := db.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	db.Close()

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := db.GetMemory("m1")
	if err != nil || m.Text != "old text" || m.Quote != "" {
		t.Fatalf("migrated row: %+v, %v", m, err)
	}
	var cols int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('memory')`).Scan(&cols); err != nil || cols != 9 {
		t.Fatalf("memory columns = %d, %v; want 9", cols, err)
	}
	addMem(t, db, "project", "new text") // no trigger left to write the dropped index
}

func TestMemoryValidation(t *testing.T) {
	db := openTest(t)
	for _, m := range []*Memory{
		{Text: "t", Origin: "user"},
		{Scope: "user", Origin: "user"},
		{Scope: "user", Text: "t"},
	} {
		if err := db.AddMemory(m); err == nil {
			t.Fatalf("want error for %+v", m)
		}
	}
}
