package hostapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

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
	if err := h.Settings.SetHost("memory.autonomy", "ask"); err != nil {
		t.Fatal(err)
	}

	// ask: every proposal queues, nothing is stored.
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

	runExpectErr(t, r, `zemi.memory.add({ kind = "rumor", text = "x" })`, "kind outside the enum")

	// User config before the first send is its own provenance, and what it
	// writes is the user's memory, not zemi's.
	h2, r2 := newTestHostDB(t)
	if err := h2.Settings.SetHost("memory.autonomy", "ask"); err != nil {
		t.Fatal(err)
	}
	runUser(t, r2, `
		local out = zemi.memory.add({ kind = "fact", text = "x" })
		assert(out.queued == true, "add before a session must queue")
	`)
	if p := h2.memoryProposals(); len(p) != 1 || p[0].Origin != learn.OriginUser {
		t.Fatalf("proposal from user config: %+v", p)
	}

	// Auto mode in a trusted project stores a plain fact and returns its id.
	if err := h.Settings.SetHost("memory.autonomy", "auto"); err != nil {
		t.Fatalf("SetHost autonomy: %v", err)
	}
	h.SetProjectTrusted(true)
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

// TestMemoryAutoQueuesOutsideText: auto autonomy still queues in an
// untrusted project and in a session that it or a subagent fed text from
// outside the machine, and there a model's memory edit asks even in auto mode.
func TestMemoryAutoQueuesOutsideText(t *testing.T) {
	for _, tc := range []struct {
		name         string
		untrusted    bool
		tool, inTool string // a tool the session called, one its subagent called
		stored, fork bool
	}{
		{name: "trusted, local tools", tool: "bash", inTool: "read", stored: true},
		{name: "untrusted project", untrusted: true},
		{name: "fetch in the session", tool: "fetch"},
		{name: "web search in a subagent", inTool: "web_search"},
		{name: "mcp tool", tool: "mcp_github_search"},
		{name: "lua-registered tool", tool: "jira_lookup"},
		{name: "forked after a fetch", tool: "fetch", fork: true},
		{name: "forked after a subagent's web search", tool: "agent", inTool: "web_search", fork: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			h.Settings.SetHost("memory.autonomy", "auto")
			h.SetProjectTrusted(!tc.untrusted)
			sid := seedSession(t, h)
			child := store.NewID()
			if err := h.DB.CreateSession(child, sid, "sub", "web"); err != nil {
				t.Fatal(err)
			}
			if tc.tool != "" {
				h.recordToolCall(sid, "c1", tc.tool, "", true)
			}
			if tc.inTool != "" {
				h.recordToolCall(child, "c2", tc.inTool, "", true)
			}
			if tc.fork {
				if _, err := h.ForkSession(); err != nil {
					t.Fatal(err)
				}
			}
			stored, _, err := h.proposeMemory(h.AgentSessionID(), learn.OriginLearned, "project", "fact", "the build uses make", nil)
			if err != nil || stored != tc.stored {
				t.Fatalf("stored = %v, want %v (%v)", stored, tc.stored, err)
			}
			// the edit auto autonomy does not apply goes to the user; no mode settles it
			if err := h.SetPermMode("auto"); err != nil {
				t.Fatal(err)
			}
			res, err := h.storeMemory(h.AgentSessionID(), "project", learn.OriginLearned, "fact", "tests run with make test", nil)
			if err == nil {
				err = h.applyProposal(res)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.modelEditMemory(h.AgentSessionID(), res.Memory.ID, "tests run with go test"); (err == nil) != tc.stored {
				t.Fatalf("edit err = %v, want applied %v", err, tc.stored)
			}
		})
	}
}

// TestMemorySearchFromLua covers search rows, scope merging and the hit
// bump (DESIGN §7).
func TestMemorySearchFromLua(t *testing.T) {
	h, r := newTestHostDB(t)
	seedSession(t, h)

	res, err := h.storeMemory(h.AgentSessionID(), "project", learn.OriginLearned, "gotcha",
		"postgres FTS triggers keep the index in sync", nil)
	if err != nil {
		t.Fatalf("intake project: %v", err)
	}
	if err := h.applyProposal(res); err != nil {
		t.Fatalf("apply: %v", err)
	}
	resU, err := h.storeMemory(h.AgentSessionID(), "user", learn.OriginUser, "preference",
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
	rows, err := h.memorySearch("all", "widget", 4, true)
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

	res, err := h.storeMemory(h.AgentSessionID(), "project", learn.OriginLearned, "fact", "old note about the pipeline", nil)
	if err != nil {
		t.Fatalf("intake: %v", err)
	}
	if err := h.applyProposal(res); err != nil {
		t.Fatalf("apply: %v", err)
	}
	res2, err := h.storeMemory(h.AgentSessionID(), "project", learn.OriginLearned, "fact", "new note about the pipeline", nil)
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
		if err == nil || !strings.Contains(err.Error(), "only user config") {
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

	runUser(t, r, `
		local ok, err = zemi.memory.forget("nope")
		assert(ok == nil and err:find("no such memory"), tostring(err))
		ok, err = zemi.memory.supersede("`+newID+`", "ghost")
		assert(ok == nil and err:find("no such memory"), tostring(err))
	`)
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

	if got := h.digestFor(""); !strings.Contains(got, "later note") {
		t.Fatalf("digestFor missed the new memory: %q", got)
	}
}

// TestDigestRanksByFirstMessage: an old memory about the first message's
// topic makes a digest that newer memories fill, and common words alone
// lift nothing.
func TestDigestRanksByFirstMessage(t *testing.T) {
	h, _ := newTestHostDB(t)
	add := func(text string) {
		if err := h.DB.AddMemory(&store.Memory{Scope: "project", Kind: "fact", Text: text, ProvenanceSessionID: "seed", Origin: learn.OriginLearned}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // distinct updated_at, so newest-first is well defined
	}
	add("make golden regenerates the UI golden files")
	for i := range 15 {
		add(fmt.Sprintf("the build step %d runs the tests for the project", i))
	}
	for _, tc := range []struct {
		first string
		want  bool
	}{
		{"", false},
		{"can you fix the tests for the project", false},
		{"regenerate the golden files", true},
	} {
		if got := strings.Contains(h.digestFor(tc.first), "UI golden files"); got != tc.want {
			t.Errorf("first message %q: golden memory in digest = %v", tc.first, got)
		}
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
	h.openMemoryReview()
	if len(ui.reviewItems) != 1 || len(ui.reviewItems[0]) != 2 {
		t.Fatalf("review rows: %+v", ui.reviewItems)
	}
	if !ui.reviewItems[0][0].Queued || ui.reviewItems[0][0].Origin != learn.OriginUser {
		t.Fatalf("first row: %+v", ui.reviewItems[0][0])
	}
	if ui.reviewItems[0][1].Origin != learn.OriginUser {
		t.Fatalf("second row: %+v", ui.reviewItems[0][1])
	}
	if rows := projectMemories(t, h); len(rows) != 1 {
		t.Fatalf("no-mark review changed the store: %d rows", len(rows))
	}

	// Second open: move the remembered note everywhere, with its age and
	// hits, keep the proposal here.
	if hit, _ := h.DB.SearchMemory("project", "remembered", 5, true); len(hit) != 1 {
		t.Fatalf("search hits: %+v", hit)
	}
	before := projectMemories(t, h)[0]
	moved := slices.Clone(ui.reviewItems[0])
	moved[1].Scope = "user"
	ui.reviewResult = moved
	h.openMemoryReview()
	if rows := projectMemories(t, h); len(rows) != 0 {
		t.Fatalf("moved note still in the project: %+v", rows)
	}
	if rows, _ := h.GlobalDB.ListMemory("user", false); len(rows) != 1 || rows[0].ID != moved[1].ID ||
		rows[0].UpdatedAt != before.UpdatedAt || rows[0].HitCount != 1 || rows[0].LastHitAt != before.LastHitAt {
		t.Fatalf("moved note not in the user store as it was (%+v): %+v", before, rows)
	}

	// Third open: accept the proposal, delete the moved note.
	ui.reviewResult = slices.Clone(ui.reviewItems[1])
	ui.reviewResult[0].Action = "accept"
	ui.reviewResult[1].Action = "delete"
	h.openMemoryReview()
	if queued := len(h.memoryProposals()); queued != 0 {
		t.Fatalf("proposals left: %d", queued)
	}
	if rows := projectMemories(t, h); len(rows) != 1 || rows[0].Text != "queued proposal text" {
		t.Fatalf("after accept+delete: %+v", rows)
	}
	if rows, _ := h.GlobalDB.ListMemory("user", false); len(rows) != 0 {
		t.Fatalf("delete missed the user store: %+v", rows)
	}
}

// TestMemoryReviewMovesProposals: a suggestion moved to the other scope
// waits there under its id and is stored there when kept; one that
// replaces a memory still retires it from the scope it left.
func TestMemoryReviewMovesProposals(t *testing.T) {
	h, r := newTestHostDB(t)
	seedSession(t, h)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.Remember("deploy by hand"); err != nil {
		t.Fatal(err)
	}
	old := projectMemories(t, h)[0]
	runUser(t, r, `zemi.memory.add({ kind = "fact", text = "deploy runs bin/deploy" })`)
	p := h.takeProposal(h.memoryProposals()[0].ID)
	p.Supersedes = old.ID
	if err := h.addProposal(*p); err != nil {
		t.Fatal(err)
	}
	runUser(t, r, `zemi.memory.add({ kind = "gotcha", text = "run make check first" })`)

	h.openMemoryReview()
	rows := slices.Clone(ui.reviewItems[0])
	for i := range rows {
		if rows[i].Queued {
			rows[i].Scope = "user"
			if strings.Contains(rows[i].Text, "(replaces ") {
				rows[i].Action = "accept"
			}
		}
	}
	ui.reviewResult = rows
	h.openMemoryReview()
	if live, _ := h.DB.ListMemory("project", false); len(live) != 0 {
		t.Fatalf("the replaced memory is still live: %+v", live)
	}
	moved := h.memoryProposals()
	if len(moved) != 1 || moved[0].Scope != "user" {
		t.Fatalf("moved suggestion: %+v", moved)
	}
	if got, _ := h.GlobalDB.Proposals("memory"); len(got) != 1 || got[0].ID != moved[0].ID {
		t.Fatalf("moved suggestion not in the user store: %+v", got)
	}

	ui.reviewResult = []MemoryRow{{ID: moved[0].ID, Scope: "user", Action: "accept"}}
	h.openMemoryReview()
	if live, _ := h.GlobalDB.ListMemory("user", false); len(live) != 2 {
		t.Fatalf("kept suggestion not stored everywhere: %+v", live)
	}
}

// TestMemoryReviewMoveOntoDuplicate: the destination scope already holds
// the text, so the suggestion stays where it was instead of vanishing.
func TestMemoryReviewMoveOntoDuplicate(t *testing.T) {
	h, _ := newTestHostDB(t)
	seedSession(t, h)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.GlobalDB.AddMemory(&store.Memory{ID: "m1", Scope: "user", Kind: "convention", Text: "the build runs with make check", ProvenanceSessionID: "seed", Origin: learn.OriginUser}); err != nil {
		t.Fatal(err)
	}
	p := memoryProposal{Memory: store.Memory{ID: "p1", Scope: "project", Kind: "convention", Text: "the build runs with make check", ProvenanceSessionID: "seed", Origin: learn.OriginLearned}}
	if err := h.addProposal(p); err != nil {
		t.Fatal(err)
	}

	ui.reviewResult = []MemoryRow{{ID: "p1", Scope: "user"}}
	h.openMemoryReview()
	got := h.memoryProposals()
	if len(got) != 1 || got[0].ID != "p1" || got[0].Scope != "project" {
		t.Fatalf("suggestion after a refused move: %+v", got)
	}
}

// TestEditMemory: e in /memory saves the editor's text as the user's own,
// in place; empty, unchanged or refused text changes nothing.
func TestEditMemory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, row, text string
		want            string // "" when nothing may change
	}{
		{"stored reworded", "m1", "deploy with make ship", "deploy with make ship"},
		{"stored unchanged", "m1", "deploy   by hand\n", ""},
		{"empty file", "m1", "\n", ""},
		{"secret blob refused", "m1", "AKIAIOSFODNN7EXAMPLE", ""},
		{"duplicate of another memory", "m1", "Other note", ""},
		{"suggestion reworded is kept as yours", "p1", "deploy with make ship", "deploy with make ship"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			seedSession(t, h)
			for _, m := range []*store.Memory{
				{ID: "m1", Scope: "project", Kind: "gotcha", Text: "deploy by hand", Tags: []string{"ops"}, ProvenanceSessionID: "seed", Origin: learn.OriginLearned},
				{ID: "m2", Scope: "project", Kind: "fact", Text: "other note", ProvenanceSessionID: "seed", Origin: learn.OriginLearned},
			} {
				if err := h.DB.AddMemory(m); err != nil {
					t.Fatal(err)
				}
			}
			p := memoryProposal{Memory: store.Memory{ID: "p1", Scope: "project", Kind: "gotcha", Text: "suggested note", Tags: []string{"ops"}, ProvenanceSessionID: "seed", Origin: learn.OriginLearned}}
			if err := h.addProposal(p); err != nil {
				t.Fatal(err)
			}
			ui := &fakeUI{edit: func(path string) { _ = os.WriteFile(path, []byte(tc.text), 0o600) }}
			h.editMemory(ui, tc.row)

			m, err := h.DB.GetMemory(tc.row)
			switch {
			case tc.want == "" && tc.row == "m1":
				if m.Text != "deploy by hand" || m.Origin != learn.OriginLearned {
					t.Fatalf("changed: %+v", m)
				}
			case err != nil || m.Text != tc.want || m.Origin != learn.OriginUser || m.Kind != "gotcha" || !slices.Equal(m.Tags, []string{"ops"}):
				t.Fatalf("edited: %+v, %v", m, err)
			case tc.row == "p1" && len(h.memoryProposals()) != 0:
				t.Fatal("the edited suggestion is still queued")
			}
		})
	}
}

// TestMoveMergeKeepsUserOrigin: moving a user's memory onto a learned copy
// of the same text leaves that copy user-attested.
func TestMoveMergeKeepsUserOrigin(t *testing.T) {
	h, _ := newTestHostDB(t)
	seedSession(t, h)
	if err := h.Remember("deploy by hand"); err != nil {
		t.Fatal(err)
	}
	learned := &store.Memory{Scope: "user", Kind: "fact", Text: "Deploy by hand", ProvenanceSessionID: "seed", Origin: learn.OriginLearned}
	if err := h.GlobalDB.AddMemory(learned); err != nil {
		t.Fatal(err)
	}
	if err := h.moveMemory(projectMemories(t, h)[0].ID, "project", "user"); err != nil {
		t.Fatal(err)
	}
	if live, _ := h.GlobalDB.ListMemory("user", false); len(live) != 1 || live[0].Origin != learn.OriginUser || len(projectMemories(t, h)) != 0 {
		t.Fatalf("merge: %+v", live)
	}
}

func TestAcceptProposalKeepsItOnStoreError(t *testing.T) {
	h, _ := newTestHostDB(t)
	// no origin: the store refuses the row, after the proposal was taken
	payload, _ := json.Marshal(memoryProposal{ID: "p1", Scope: "project", Kind: "fact", Text: "unstorable"})
	if err := h.DB.AddProposal(&store.Proposal{ID: "p1", Kind: "memory", Payload: string(payload)}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.acceptProposal("p1", ""); err == nil {
		t.Fatal("accept stored a memory without origin")
	}
	if props := h.memoryProposals(); len(props) != 1 || props[0].ID != "p1" {
		t.Fatalf("proposal lost: %+v", props)
	}
}

// TestMemoryCommandsRegistered pins the /memory command step.
func TestMemoryCommandsRegistered(t *testing.T) {
	h, _ := newTestHostDB(t)
	if err := h.BuiltinCommandsStep().Run(context.Background(), h.RT, &lua.Generation{ID: 1}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if h.Commands.Lookup("memory") == nil {
		t.Fatal("memory commands not registered")
	}
	if err := h.memoryCommand("forget"); err == nil {
		t.Fatal("forget without an id must fail")
	}
}
