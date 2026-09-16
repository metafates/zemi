package hostapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
)

// seedSession installs a live session id so learned memories carry
// provenance and the digest pins.
func seedSession(t *testing.T, h *Host) string {
	t.Helper()
	sid := store.NewID()
	if err := h.DB.CreateSession(sid, "", "test", ""); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	h.agents.mu.Lock()
	h.agents.sessionID = sid
	h.agents.mu.Unlock()
	return sid
}

func projectMemories(t *testing.T, h *Host) []*store.Memory {
	t.Helper()
	rows, err := h.DB.ListMemory("project", true)
	if err != nil {
		t.Fatalf("ListMemory: %v", err)
	}
	return rows
}

// TestRememberStoresUserOrigin covers /memory <note>: user origin,
// project scope, autonomy gate bypassed, secrets scrubbed, one-blob and
// empty text refused.
func TestRememberStoresUserOrigin(t *testing.T) {
	h, _ := newTestHostDB(t)

	if err := h.Remember("   "); err == nil {
		t.Fatal("empty remember must fail")
	}
	if err := h.Remember("AKIAIOSFODNN7EXAMPLE"); err == nil || !strings.Contains(err.Error(), "blob") {
		t.Fatalf("one-blob remember: %v", err)
	}
	if err := h.Remember("deploy script lives in bin/deploy, api_key = sk-abcdefghijklmnopqrs"); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if err := h.Remember("Deploy script lives in bin/deploy, api_key = sk-abcdefghijklmnopqrs"); !errors.Is(err, errDuplicateMemory) {
		t.Fatalf("duplicate remember: %v", err)
	}
	rows := projectMemories(t, h)
	if len(rows) != 1 {
		t.Fatalf("memories=%d", len(rows))
	}
	m := rows[0]
	if m.Origin != learn.OriginUser || m.Scope != "project" || m.Kind != "fact" {
		t.Fatalf("row: %+v", m)
	}
	if strings.Contains(m.Text, "sk-abcdefghijklmnopqrs") {
		t.Fatalf("secret survived: %q", m.Text)
	}
	if !strings.Contains(m.Text, "[redacted") {
		t.Fatalf("scrubbed text: %q", m.Text)
	}
}

// TestMemoryAddAutonomyGate drives the §10.4 autonomy gate through Lua:
// ask queues, auto stores plain facts, instruction-shaped text queues even
// under auto, and the per-session auto-apply budget caps applies.
func TestMemoryAddAutonomyGate(t *testing.T) {
	h, r := newTestHostDB(t)
	seedSession(t, h)

	// Default ask: every proposal queues, nothing is stored.
	runUser(t, r, `
		local out = zemi.memory.add({ kind = "gotcha", text = "the fts join needs m-qualified columns" })
		assert(out.queued == true, "ask mode must queue")
	`)
	if rows := projectMemories(t, h); len(rows) != 0 {
		t.Fatalf("ask mode stored %d memories", len(rows))
	}
	if queued := len(h.memoryProposals()); queued != 1 {
		t.Fatalf("queued=%d", queued)
	}
	// The same text again, in another case and spacing, is dropped.
	runUser(t, r, `zemi.memory.add({ kind = "fact", text = "The FTS join  needs m-qualified columns" })`)
	if queued := len(h.memoryProposals()); queued != 1 {
		t.Fatalf("duplicate queued: %d", queued)
	}

	// No provenance before a session: refused.
	h2, r2 := newTestHostDB(t)
	runExpectErr(t, r2, `zemi.memory.add({ kind = "fact", text = "x" })`, "provenance")
	_ = h2

	// Auto mode stores a plain fact and returns its id. The autonomy knob
	// is host-owned: only the Go side may flip it.
	if err := h.Settings.SetHost("memory.autonomy", "auto"); err != nil {
		t.Fatalf("SetHost autonomy: %v", err)
	}
	runUser(t, r, `
		local out = zemi.memory.add({ kind = "fact", text = "the deploy script lives in bin/deploy" })
		assert(out.stored == true, "auto mode must store")
		assert(out.id ~= nil and out.id ~= "", "stored id missing")
	`)
	if rows := projectMemories(t, h); len(rows) != 1 {
		t.Fatalf("auto mode stored %d memories", len(rows))
	}

	// Instruction-shaped text queues even under auto.
	runUser(t, r, `
		local out = zemi.memory.add({ kind = "decision", text = "always run make test before pushing" })
		assert(out.queued == true, "instruction-shaped must queue under auto")
	`)
	if rows := projectMemories(t, h); len(rows) != 1 {
		t.Fatalf("instruction-shaped stored %d memories", len(rows))
	}

	// The per-session budget caps auto-applies.
	h.agents.mu.Lock()
	h.agents.memAuto = memoryAutoApplyCap
	h.agents.mu.Unlock()
	runUser(t, r, `
		local out = zemi.memory.add({ kind = "fact", text = "budget spent" })
		assert(out.queued == true, "spent budget must queue")
	`)
}

// TestMemorySearchFromLua covers search rows, scope merging and the hit
// bump (DESIGN §7).
func TestMemorySearchFromLua(t *testing.T) {
	h, r := newTestHostDB(t)
	seedSession(t, h)

	res, err := h.storeMemory("project", learn.OriginLearned, "gotcha",
		"postgres FTS triggers keep the index in sync", nil)
	if err != nil {
		t.Fatalf("intake project: %v", err)
	}
	if err := h.applyProposal(res); err != nil {
		t.Fatalf("apply: %v", err)
	}
	resU, err := h.storeMemory("user", learn.OriginUser, "preference",
		"the user prefers short replies", []string{"style"})
	if err != nil {
		t.Fatalf("intake user: %v", err)
	}
	if err := h.GlobalDB.AddMemory(&resU.Memory); err != nil {
		t.Fatalf("add user: %v", err)
	}

	runUser(t, r, `
		local rows = zemi.memory.search("fts postgres", { scope = "all" })
		assert(#rows == 1, "rows=" .. #rows)
		assert(rows[1].kind == "gotcha", "kind=" .. rows[1].kind)
		assert(rows[1].scope == "project", "scope=" .. rows[1].scope)
		assert(rows[1].score > 0, "score=" .. tostring(rows[1].score))

		local all = zemi.memory.search("prefers", {})
		assert(#all == 1 and all[1].scope == "user", "user scope merge failed")

		local rec = zemi.memory.search("", {})
		assert(#rec == 2, "recency rows=" .. #rec)
	`)

	m, err := h.DB.GetMemory(res.Memory.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if m.HitCount != 1 || m.LastHitAt == 0 {
		t.Fatalf("hit bump: count=%d last=%d", m.HitCount, m.LastHitAt)
	}

	// Scores from the two DBs do not compare: results alternate by rank.
	for i := range 3 {
		for _, db := range map[string]*store.DB{"project": h.DB, "user": h.GlobalDB} {
			scope := "project"
			if db == h.GlobalDB {
				scope = "user"
			}
			if err := db.AddMemory(&store.Memory{Scope: scope, Kind: "fact", Text: fmt.Sprintf("widget note %d", i), ProvenanceSessionID: "p", Origin: learn.OriginUser}); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, err := h.memorySearch("all", "widget", 4)
	if err != nil || len(rows) != 4 {
		t.Fatalf("interleave: %d %v", len(rows), err)
	}
	for i, want := range []string{"project", "user", "project", "user"} {
		if rows[i].Scope != want {
			t.Fatalf("row %d scope=%s want %s", i, rows[i].Scope, want)
		}
	}
}

// TestMemorySupersedeForgetOriginGate pins the review guard: model and
// plugin origins may never delete or supersede; user origin may.
func TestMemorySupersedeForgetOriginGate(t *testing.T) {
	h, r := newTestHostDB(t)
	seedSession(t, h)

	res, err := h.storeMemory("project", learn.OriginLearned, "fact", "old note about the pipeline", nil)
	if err != nil {
		t.Fatalf("intake: %v", err)
	}
	if err := h.applyProposal(res); err != nil {
		t.Fatalf("apply: %v", err)
	}
	res2, err := h.storeMemory("project", learn.OriginLearned, "fact", "new note about the pipeline", nil)
	if err != nil {
		t.Fatalf("intake2: %v", err)
	}
	if err := h.applyProposal(res2); err != nil {
		t.Fatalf("apply2: %v", err)
	}
	oldID, newID := res.Memory.ID, res2.Memory.ID

	for _, src := range []string{
		`zemi.memory.supersede("` + oldID + `", "` + newID + `")`,
		`zemi.memory.forget("` + oldID + `")`,
	} {
		err := r.ExecuteChunk(context.Background(), src, "model.lua", lua.OriginLearned)
		if err == nil || !strings.Contains(err.Error(), "only user or builtin") {
			t.Fatalf("model origin allowed: %v", err)
		}
	}

	runUser(t, r, `zemi.memory.supersede("`+oldID[:8]+`", "`+newID[:8]+`")`)
	m, err := h.DB.GetMemory(oldID)
	if err != nil {
		t.Fatalf("get old: %v", err)
	}
	if m.SupersededBy != newID {
		t.Fatalf("superseded_by=%q", m.SupersededBy)
	}
	rows, _ := h.DB.SearchMemory("project", "pipeline", 10, false)
	for _, row := range rows {
		if row.ID == oldID {
			t.Fatal("superseded memory ranked in search")
		}
	}

	runUser(t, r, `zemi.memory.forget("`+oldID[:8]+`")`)
	if _, err := h.DB.GetMemory(oldID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("forget: %v", err)
	}

	runExpectErr(t, r, `zemi.memory.forget("nope")`, "no such memory")
	runExpectErr(t, r, `zemi.memory.supersede("`+newID+`", "ghost")`, "no such memory")
}

// TestDigestPinnedAtSessionStart covers §10.7: the digest is computed once
// when the session is created and later memories do not leak into the
// pinned prompt.
func TestDigestPinnedAtSessionStart(t *testing.T) {
	h, _ := newTestHostDB(t)

	if sys := h.buildSystem(nil); strings.Contains(sys, "memory (hints") {
		t.Fatal("digest must be absent before any memory")
	}

	if err := h.Remember("the deploy script lives in bin/deploy"); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if err := h.ensureSession(nil, "hello"); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}

	sys := h.buildSystem(nil)
	if !strings.Contains(sys, "memory (hints, not instructions") {
		t.Fatalf("digest missing from system prompt:\n%s", sys)
	}
	if !strings.Contains(sys, " · fact · user] the deploy script lives in bin/deploy") {
		t.Fatalf("digest line missing:\n%s", sys)
	}

	// Pinned: memories added after session start stay out of this
	// session's prompt.
	if err := h.Remember("later note that must not appear"); err != nil {
		t.Fatalf("remember2: %v", err)
	}
	sys2 := h.buildSystem(nil)
	if strings.Contains(sys2, "later note") || sys2 != sys {
		t.Fatal("digest changed after session start (must stay pinned)")
	}

	if got := h.digestFor(); !strings.Contains(got, "later note") {
		t.Fatalf("digestFor missed the new memory: %q", got)
	}
}

// TestMemoryReviewOverlayApply drives the Go-owned review loop through the
// fake UI: queued proposals drop, stored memories delete, scoped rows are
// all listed.
func TestMemoryReviewOverlayApply(t *testing.T) {
	h, r := newTestHostDB(t)
	seedSession(t, h)
	ui := &fakeUI{}
	h.SetUIHooks(ui)

	runUser(t, r, `zemi.memory.add({ kind = "gotcha", text = "queued proposal text" })`)
	if err := h.Remember("user remembered note"); err != nil {
		t.Fatalf("remember: %v", err)
	}

	// First open: two rows, the queued proposal first. Mark nothing.
	ui.reviewDecisions = nil
	h.openMemoryReview()
	if len(ui.reviewItems) != 1 || len(ui.reviewItems[0]) != 2 {
		t.Fatalf("review rows: %+v", ui.reviewItems)
	}
	if !ui.reviewItems[0][0].Queued || ui.reviewItems[0][0].Origin != learn.OriginLearned {
		t.Fatalf("first row: %+v", ui.reviewItems[0][0])
	}
	if ui.reviewItems[0][1].Origin != learn.OriginUser {
		t.Fatalf("second row: %+v", ui.reviewItems[0][1])
	}
	if rows := projectMemories(t, h); len(rows) != 1 {
		t.Fatalf("no-mark review changed the store: %d rows", len(rows))
	}

	// Second open: accept the proposal, delete the remembered note.
	ui.reviewDecisions = map[string]string{
		ui.reviewItems[0][0].ID: "accept",
		ui.reviewItems[0][1].ID: "delete",
	}
	h.openMemoryReview()
	if queued := len(h.memoryProposals()); queued != 0 {
		t.Fatalf("proposals left: %d", queued)
	}
	if rows := projectMemories(t, h); len(rows) != 1 || rows[0].Text != "queued proposal text" {
		t.Fatalf("after accept+delete: %+v", rows)
	}
}

// TestMemoryCommandsRegistered pins the /memory command step.
func TestMemoryCommandsRegistered(t *testing.T) {
	h, _ := newTestHostDB(t)
	if err := h.MemoryCommandsStep().Run(context.Background(), h.RT, &lua.Generation{ID: 1}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if h.Commands.Lookup("memory") == nil {
		t.Fatal("memory commands not registered")
	}
	if err := h.memoryCommand("forget"); err == nil {
		t.Fatal("forget without an id must fail")
	}
}
