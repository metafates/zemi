package store

import (
	"errors"
	"testing"
)

// seedMemory inserts a row the way the pre-brain store's write path did,
// the frozen table's only writer now (DESIGN §7).
func seedMemory(t *testing.T, db *DB, m *Memory) {
	t.Helper()
	_, err := db.db.Exec(`INSERT INTO memory
		(id, scope, text, quote, provenance_session_id, created_at, updated_at, superseded_by, origin)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Scope, m.Text, m.Quote, nullString(m.ProvenanceSessionID), m.CreatedAt, m.UpdatedAt,
		nullString(m.SupersededBy), m.Origin)
	if err != nil {
		t.Fatal(err)
	}
}

func TestMemoryReads(t *testing.T) {
	db := openTest(t)
	gate := &Memory{
		ID: "gate1aaa", Scope: ScopeProject, Text: "the deploy gate lives in ci/pipeline", Quote: "q",
		ProvenanceSessionID: "s9", CreatedAt: 3, UpdatedAt: 3, Origin: OriginUser,
	}
	rust := &Memory{ID: "rust1aaa", Scope: ScopeUser, Text: "prefers rust over go for tools", CreatedAt: 1, UpdatedAt: 1, Origin: OriginUser}
	old := &Memory{
		ID: "old01aaa", Scope: ScopeProject, Text: "gate is at ci/old", CreatedAt: 2, UpdatedAt: 2,
		Origin: OriginLearned, SupersededBy: "gate1aaa",
	}
	for _, m := range []*Memory{gate, rust, old} {
		seedMemory(t, db, m)
	}

	m, err := db.GetMemory(rust.ID)
	if err != nil || m.Text != rust.Text || m.Quote != "" || m.ProvenanceSessionID != "" {
		t.Fatalf("get: %+v err=%v", m, err)
	}
	// a superseded row resolves, the trail /memory's collapsed group shows
	if m, err = db.GetMemory(old.ID); err != nil || m.SupersededBy != gate.ID {
		t.Fatalf("superseded row: %+v err=%v", m, err)
	}
	if list, _ := db.ListMemory(ScopeProject, false); len(list) != 1 || list[0].ID != gate.ID {
		t.Fatalf("live list = %+v", list)
	}
	if list, _ := db.ListMemory(ScopeProject, true); len(list) != 2 || list[0].ID != gate.ID || list[1].ID != old.ID {
		t.Fatalf("full list = %+v, want newest first", list)
	}
	if _, err := db.GetMemory("nonesuch"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
