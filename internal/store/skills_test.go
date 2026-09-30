package store

import (
	"errors"
	"strings"
	"testing"
)

func TestSyncSkillsAndEnabledState(t *testing.T) {
	db := openTest(t)
	all := []*Skill{
		{Name: "user-one", Path: "/a/user-one", Source: "user"},
		{Name: "builtin-one", Path: "/a/builtin-one", Source: "builtin"},
	}
	if err := db.SyncSkills(all); err != nil {
		t.Fatalf("sync: %v", err)
	}
	rows, err := db.ListSkills()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d", len(rows))
	}
	byName := map[string]*Skill{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if !byName["user-one"].Enabled || !byName["builtin-one"].Enabled {
		t.Fatal("skills must start enabled")
	}

	if err := db.SetSkillEnabled("builtin-one", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := db.SetSkillEnabled("nope", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown enable err=%v", err)
	}

	// A resync refreshes paths and drops vanished skills but keeps the
	// stored enabled choice.
	if err := db.SyncSkills([]*Skill{
		{Name: "user-one", Path: "/b/user-one", Source: "user"},
		{Name: "builtin-one", Path: "/b/builtin-one", Source: "builtin"},
	}); err != nil {
		t.Fatalf("resync: %v", err)
	}
	rows, err = db.ListSkills()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d", len(rows))
	}
	for _, r := range rows {
		if !strings.HasPrefix(r.Path, "/b/") {
			t.Fatalf("path not refreshed: %+v", r)
		}
		if r.Enabled != (r.Name == "user-one") {
			t.Fatalf("resync must keep the enabled choice: %+v", r)
		}
	}

	// Syncing with an empty set wipes the table.
	if err := db.SyncSkills(nil); err != nil {
		t.Fatalf("empty sync: %v", err)
	}
	if rows, _ := db.ListSkills(); len(rows) != 0 {
		t.Fatalf("rows after empty sync=%d", len(rows))
	}
}
