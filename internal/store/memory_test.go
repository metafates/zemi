package store

import (
	"errors"
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
