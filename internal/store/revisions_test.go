package store

import (
	"errors"
	"testing"
)

func TestConfigRevisionsCRUD(t *testing.T) {
	db, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rev := &ConfigRevision{
		ID:          NewID(),
		Title:       "rules: deny tool.bash rm -rf *",
		ChangesJSON: `{"subject":"tool.bash"}`,
		PathsJSON:   `["permissions.lua"]`,
		Status:      "pending",
		AppliedBy:   "user",
	}
	if err := db.AddRevision(rev); err != nil {
		t.Fatalf("add: %v", err)
	}
	if rev.CreatedAt == 0 {
		t.Fatal("CreatedAt must default to now")
	}

	got, err := db.GetRevision(rev.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != rev.Title || got.Status != "pending" || got.AppliedBy != "user" {
		t.Fatalf("row: %+v", got)
	}

	if err := db.SetRevisionStatus(rev.ID, "applied"); err != nil {
		t.Fatalf("status: %v", err)
	}
	rows, err := db.ListRevisions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != "applied" {
		t.Fatalf("rows: %+v", rows)
	}

	if err := db.SetRevisionStatus("missing", "applied"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing status: %v", err)
	}
	if _, err := db.GetRevision("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing get: %v", err)
	}
	if err := db.AddRevision(&ConfigRevision{}); err == nil {
		t.Fatal("empty id must be refused")
	}
}
