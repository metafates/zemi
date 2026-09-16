package store

import (
	"errors"
	"strings"
	"testing"
)

func TestSyncSkillsAndEnabledState(t *testing.T) {
	db := openTest(t)
	all := []*Skill{
		{Name: "authored-one", Path: "/a/authored-one", Source: "authored", DefaultEnabled: true},
		{Name: "learned-one", Path: "/a/learned-one", Source: "learned"},
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
	if !byName["authored-one"].Enabled {
		t.Fatal("authored skill must start enabled")
	}
	if byName["learned-one"].Enabled {
		t.Fatal("learned skill must start disabled")
	}

	if err := db.SetSkillEnabled("learned-one", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := db.SetSkillEnabled("nope", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown enable err=%v", err)
	}

	// A resync refreshes paths and drops vanished skills but keeps the
	// stored enabled choice.
	if err := db.SyncSkills([]*Skill{
		{Name: "authored-one", Path: "/b/authored-one", Source: "authored", DefaultEnabled: true},
		{Name: "learned-one", Path: "/b/learned-one", Source: "learned", DefaultEnabled: true},
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
	}
	sk, err := db.GetSkill("learned-one")
	if err != nil {
		t.Fatal(err)
	}
	if !sk.Enabled {
		t.Fatal("resync must keep the enabled choice")
	}

	if err := db.SyncSkills([]*Skill{{Name: "authored-one", Path: "/c/authored-one", Source: "learned"}}); err != nil {
		t.Fatal(err)
	}
	if sk, _ := db.GetSkill("authored-one"); sk.Enabled {
		t.Fatal("a source change must reset enabled to the new default")
	}

	// Syncing with an empty set wipes the table.
	if err := db.SyncSkills(nil); err != nil {
		t.Fatalf("empty sync: %v", err)
	}
	if rows, _ := db.ListSkills(); len(rows) != 0 {
		t.Fatalf("rows after empty sync=%d", len(rows))
	}
}
