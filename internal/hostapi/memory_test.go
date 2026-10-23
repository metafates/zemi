package hostapi

import (
	"bytes"
	"cmp"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/brain"
	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// seedSession installs a live session id so learned memories carry
// provenance and the digest pins.
func seedSession(t *testing.T, h *Host) string {
	t.Helper()
	sid := store.ShortID()
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
	var out []*store.Memory
	for _, m := range must(h.memoryList(true)) {
		if m.Scope == "project" {
			out = append(out, m)
		}
	}
	return out
}

// seedMemory writes m to its partition the way a store write did, keeping
// its id, origin and age, its quote and provenance in the ledger, the line
// placed newest first by its date.
func seedMemory(t *testing.T, h *Host, m *store.Memory) {
	t.Helper()
	if m.ID == "" {
		m.ID = store.ShortID()
	}
	p := h.partition(m.Scope)
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := brain.Line{ID: m.ID, Origin: m.Origin, Date: time.UnixMilli(m.UpdatedAt).UTC(), Text: m.Text}
	if m.UpdatedAt == 0 {
		// a row with no stamp is as new as it gets
		line.Date = time.Now().UTC()
	}
	path := filepath.Join(p.Dir, brain.MemFile)
	cur, _ := os.ReadFile(path)
	lines, _ := brain.ParseMemories(cur)
	at := slices.IndexFunc(lines, func(l brain.Line) bool { return l.Date.Before(line.Date) })
	if at < 0 {
		lines = append(lines, line)
	} else {
		lines = slices.Insert(lines, at, line)
	}
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString(l.String())
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	lg, err := p.OpenLedger()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg.Close() }()
	ts := m.UpdatedAt
	if ts == 0 {
		ts = time.Now().UnixMilli()
	}
	op := &brain.Op{
		Op: brain.OpAdd, ID: m.ID, Text: m.Text, Quote: m.Quote,
		SessionID: cmp.Or(m.ProvenanceSessionID, "seed"), Hash: brain.FileHash([]byte(m.Text)),
		Scope: m.Scope, Origin: m.Origin, TS: ts,
	}
	if err := lg.Insert(op); err != nil {
		t.Fatal(err)
	}
}

// supersededBy is the id the ledger shows as having replaced oldID, ""
// when it is still live.
func supersededBy(t *testing.T, h *Host, oldID string) string {
	t.Helper()
	p, m, err := h.locateMemory(oldID)
	if err != nil {
		return ""
	}
	lg, err := p.OpenLedger()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg.Close() }()
	trail, err := lg.ByID(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range slices.Backward(trail) {
		if op.Op == brain.OpSupersede {
			return op.SupersededBy
		}
	}
	return ""
}

// TestRememberStoresUserOrigin covers /memory <note>: user origin,
// project scope, autonomy gate bypassed, a secret and empty text refused.
func TestRememberStoresUserOrigin(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)

	if err := h.Remember("   "); err == nil {
		t.Fatal("empty remember must fail")
	}
	if err := h.Remember("deploy script lives in bin/deploy, api_key = sk-abcdefghijklmnopqrs"); !errors.Is(err, learn.ErrRefused) {
		t.Fatalf("secret remember: %v", err)
	}
	if err := h.Remember("deploy script lives in bin/deploy"); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if err := h.Remember("Deploy script lives in bin/deploy"); !errors.Is(err, errDuplicateMemory) || !strings.Contains(err.Error(), `"deploy script lives in bin/deploy"`) {
		t.Fatalf("duplicate remember: %v", err)
	}
	rows := projectMemories(t, h)
	if len(rows) != 1 {
		t.Fatalf("memories=%d", len(rows))
	}
	m := rows[0]
	if m.Origin != store.OriginUser || m.Scope != "project" {
		t.Fatalf("row: %+v", m)
	}
}

// TestMemoryUpdatedAtIsTheWrite: /memory ages a line by the clock its write ran
// on, not by the day memories.md stores.
func TestMemoryUpdatedAtIsTheWrite(t *testing.T) {
	t.Parallel()
	h, clock := newLoopHost(t)
	at := testEpoch.Add(18 * time.Hour) // the day the line stores is 18h before this
	clock.set(at)
	if err := h.Remember("deploy script lives in bin/deploy"); err != nil {
		t.Fatal(err)
	}
	rows := projectMemories(t, h)
	if len(rows) != 1 || rows[0].UpdatedAt != at.UnixMilli() {
		t.Fatalf("saved at %s, /memory says %+v", at, rows)
	}
	clock.set(at.Add(time.Hour))
	if _, err := h.rewriteMemory(rows[0].ID, "deploy script lives in bin/deploy, the ship one in bin/ship"); err != nil {
		t.Fatal(err)
	}
	if rows = projectMemories(t, h); len(rows) != 1 || rows[0].UpdatedAt != at.Add(time.Hour).UnixMilli() {
		t.Fatalf("edited at %s, /memory says %+v", at.Add(time.Hour), rows)
	}
}

// TestMemoryAddAutonomyGate drives zemi.memory.add: ask queues, a
// near-duplicate names the row it repeats, user config before a session
// writes the user's own memory, and auto stores a plain fact with its id.
func TestMemoryAddAutonomyGate(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	seedSession(t, h)
	if _, err := h.Settings.SetQuiet("memory.autonomy", "ask"); err != nil {
		t.Fatal(err)
	}
	runUser(t, r, `
		local out = zemi.memory.add("the fts join needs m-qualified columns")
		assert(out.queued == true, "ask mode must queue")
	`)
	if rows := projectMemories(t, h); len(rows) != 0 {
		t.Fatalf("ask mode stored %d memories", len(rows))
	}
	if queued := len(h.memoryProposals()); queued != 1 {
		t.Fatalf("queued=%d", queued)
	}
	runUser(t, r, `
		local out = zemi.memory.add("The FTS join  needs m-qualified columns", { scope = "user" })
		assert(out.duplicate == "`+h.memoryProposals()[0].ID+`", "duplicate=" .. tostring(out.duplicate))
	`)
	runExpectErr(t, r, `zemi.memory.add("x", { scope = "galaxy" })`, "scope must be project or user")

	h2, r2 := newTestHostDB(t)
	if _, err := h2.Settings.SetQuiet("memory.autonomy", "ask"); err != nil {
		t.Fatal(err)
	}
	runUser(t, r2, `
		local out = zemi.memory.add("x")
		assert(out.queued == true, "add before a session must queue")
	`)
	if p := h2.memoryProposals(); len(p) != 1 || p[0].Origin != store.OriginUser {
		t.Fatalf("proposal from user config: %+v", p)
	}

	if _, err := h.Settings.SetQuiet("memory.autonomy", "auto"); err != nil {
		t.Fatalf("set autonomy: %v", err)
	}
	h.SetProjectTrusted(true)
	runUser(t, r, `
		local out = zemi.memory.add("the deploy script lives in bin/deploy")
		assert(out.stored == true, "auto mode must store")
		assert(out.id ~= nil and out.id ~= "", "stored id missing")
		local rows = zemi.memory.list()
		assert(#rows == 1 and rows[1].quote == "", "list: " .. #rows)
	`)
}

// TestMemoryGate pins the autonomy gate (DESIGN §10.4) through the memory
// tool: the quote rule, the near-duplicate refusal, // cut, what replaces does with and without text, and /memory over
// budget.
func TestMemoryGate(t *testing.T) {
	filler := func(i int) string { return strings.TrimSpace(strings.Repeat(fmt.Sprintf("w%02d ", i), 65)) } // 259 bytes, no content token
	fill := func(t *testing.T, h *Host, n int) (oldest string) {
		t.Helper()
		for i := range n {
			m := &store.Memory{Scope: "project", Text: filler(i), ProvenanceSessionID: "seed", Origin: store.OriginLearned, UpdatedAt: int64(1 + i)}
			seedMemory(t, h, m)
			if i == 0 {
				oldest = m.ID
			}
		}
		return oldest
	}
	learned := func(t *testing.T, h *Host, scope store.Scope, text string) string {
		t.Helper()
		m := &store.Memory{Scope: scope, Text: text, ProvenanceSessionID: "seed", Origin: store.OriginLearned}
		seedMemory(t, h, m)
		return m.ID
	}
	for _, tc := range []struct {
		name        string
		autonomy    string
		seed        func(t *testing.T, h *Host, sid string) (replaces string)
		scope       store.Scope
		text, quote string
		note        bool   // /memory <note> instead of the tool
		refused     bool   // nothing was stored, so the answer is an error
		want        string // in the answer or the error
		check       func(t *testing.T, h *Host, ui *fakeUI, replaces string)
	}{
		{name: "project fact stores", text: "the deploy script lives in bin/deploy", want: "saved as "},
		{name: "ask queues", autonomy: "ask", text: "the deploy script lives in bin/deploy", want: "queued for the user's review"},
		{name: "instruction shape stores", text: "always run make check before a commit", want: "saved as "},
		{name: "user fact without a quote stores", scope: "user", text: "the user wants short replies", want: "saved as "},
		{
			name: "quote in a user row stores", scope: "user", text: "the user wants short replies",
			seed: func(t *testing.T, h *Host, sid string) string {
				addText(t, h, sid, "user", "Please,  keep replies SHORT from now on")
				return ""
			},
			quote: "keep replies short", want: "saved as ",
		},
		{
			name: "quote the user never wrote drops", scope: "user", text: "the user wants short replies",
			seed: func(t *testing.T, h *Host, sid string) string {
				addText(t, h, sid, "user", "make the tests pass")
				return ""
			},
			quote: "keep replies short", refused: true, want: "dropped: quote not found",
		},
		{
			name: "quote in a compaction summary drops", scope: "user", text: "the user wants short replies",
			seed: func(t *testing.T, h *Host, sid string) string {
				addText(t, h, sid, "user", compactPrefix+"the user asked to keep replies short"+compactSuffix)
				return ""
			},
			quote: "keep replies short", refused: true, want: "dropped: quote not found",
		},
		{
			name: "quote only in an attachment drops", scope: "user", text: "the user wants short replies",
			seed: func(t *testing.T, h *Host, sid string) string {
				addText(t, h, sid, "user", "read @notes.md"+agent.AttachMarker+"\nkeep replies short\n</attached-files>")
				return ""
			},
			quote: "keep replies short", refused: true, want: "dropped: quote not found",
		},
		{
			name: "near-duplicate of the other scope", text: "make golden regenerates the UI golden files",
			seed: func(t *testing.T, h *Host, sid string) string {
				learned(t, h, "user", "the ui golden files are regenerated by make golden")
				return ""
			},
			refused: true, want: `(user): "the ui golden files are regenerated by make golden"; to keep both facts, pass replaces=`,
			check: func(t *testing.T, h *Host, ui *fakeUI, _ string) {
				live, _ := h.memoryList(false)
				if len(live) != 1 || live[0].Scope != "user" {
					t.Fatal("the duplicate was stored")
				}
			},
		},
		{
			name: "near-duplicate of a pending proposal is already queued", text: "make golden regenerates the UI golden files",
			seed: func(t *testing.T, h *Host, sid string) string {
				if err := h.addProposal(memoryProposal{Scope: "project", Text: "the ui golden files are regenerated by make golden", ProvenanceSessionID: sid, Origin: store.OriginLearned}); err != nil {
					t.Fatal(err)
				}
				return ""
			},
			refused: true, want: "already queued for the user's review as ",
		},
		{
			name: "a full scope cuts its oldest learned lines", text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string { return fill(t, h, 11) },
			want: "saved as ",
			check: func(t *testing.T, h *Host, ui *fakeUI, oldest string) {
				live, _ := h.memoryList(false)
				if slices.ContainsFunc(live, func(m *store.Memory) bool { return m.ID == oldest }) {
					t.Fatal("the oldest learned line survived the cut")
				}
			},
		},
		{
			name: "a full scope cuts around a user line", text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string {
				m := &store.Memory{Scope: "project", Text: filler(0), ProvenanceSessionID: "seed", Origin: store.OriginUser, UpdatedAt: 1}
				seedMemory(t, h, m)
				for i := 1; i < 11; i++ {
					seedMemory(t, h, &store.Memory{Scope: "project", Text: filler(i), ProvenanceSessionID: "seed", Origin: store.OriginLearned, UpdatedAt: int64(i + 1)})
				}
				return ""
			},
			want: "saved as ",
			check: func(t *testing.T, h *Host, ui *fakeUI, _ string) {
				live, _ := h.memoryList(false)
				if !slices.ContainsFunc(live, func(m *store.Memory) bool { return m.Origin == store.OriginUser }) {
					t.Fatal("the cut took the user's line")
				}
				if len(h.memoryTrail(live)) == 0 {
					t.Fatal("nothing was cut")
				}
			},
		},
		{
			name: "accepting into a full scope cuts around the user's line", autonomy: "ask", text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string {
				m := &store.Memory{Scope: "project", Text: filler(0), ProvenanceSessionID: "seed", Origin: store.OriginUser, UpdatedAt: 1}
				seedMemory(t, h, m)
				for i := 1; i < 11; i++ {
					seedMemory(t, h, &store.Memory{Scope: "project", Text: filler(i), ProvenanceSessionID: "seed", Origin: store.OriginLearned, UpdatedAt: int64(i + 1)})
				}
				return ""
			},
			want: "queued for the user's review",
			check: func(t *testing.T, h *Host, ui *fakeUI, _ string) {
				p := h.memoryProposals()
				if _, err := h.acceptProposal(p[0].ID); err != nil {
					t.Fatalf("accept: %v", err)
				}
				live, _ := h.memoryList(false)
				if !slices.ContainsFunc(live, func(m *store.Memory) bool { return m.Origin == store.OriginUser }) {
					t.Fatal("the accept cut the user's line")
				}
			},
		},
		{
			name: "an edit past a full scope refuses", autonomy: "ask", text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string { fill(t, h, 11); return "" },
			want: "queued for the user's review",
			check: func(t *testing.T, h *Host, ui *fakeUI, _ string) {
				for _, id := range []string{h.memoryProposals()[0].ID, projectMemories(t, h)[10].ID} {
					if _, err := h.rewriteMemory(id, filler(0)+" and more"); err == nil || !strings.Contains(err.Error(), "project memory is full") {
						t.Fatalf("edit of %s: %v", id, err)
					}
				}
				if len(h.memoryProposals()) != 1 || projectMemories(t, h)[10].Text != filler(0) {
					t.Fatal("an edit went past the budget")
				}
				// a shrinking edit is taken, so a scope already over can shrink
				if _, err := h.rewriteMemory(h.memoryProposals()[0].ID, "deploy in bin"); err != nil {
					t.Fatalf("shrinking edit: %v", err)
				}
			},
		},
		{
			name: "moving into a full scope refuses", text: "the ci runs on every push",
			seed: func(t *testing.T, h *Host, sid string) string {
				fill(t, h, 12)
				learned(t, h, "user", strings.Repeat("the deploy script needs this fact, ", 9))
				return ""
			},
			want: "saved as ",
			check: func(t *testing.T, h *Host, ui *fakeUI, _ string) {
				live, _ := h.memoryList(false)
				var uid string
				for _, m := range live {
					if m.Scope == "user" {
						uid = m.ID
					}
				}
				if err := h.moveMemory(uid, "user", "project"); err == nil || !strings.Contains(err.Error(), "project memory is full") {
					t.Fatalf("move: %v", err)
				}
				if n := len(projectMemories(t, h)); n != 11 {
					t.Fatalf("moved over budget: %d rows", n)
				}
			},
		},
		{
			name: "full scope with replaces stores", text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string { return fill(t, h, 11) },
			want: "saved as ",
			check: func(t *testing.T, h *Host, ui *fakeUI, replaces string) {
				if supersededBy(t, h, replaces) == "" {
					t.Fatal("the replaced memory is still live")
				}
			},
		},
		{
			name: "replacing the user's memory stores", text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string {
				if err := h.Remember("deploy by hand"); err != nil {
					t.Fatal(err)
				}
				return projectMemories(t, h)[0].ID
			},
			want: "saved as ",
			check: func(t *testing.T, h *Host, ui *fakeUI, replaces string) {
				if supersededBy(t, h, replaces) == "" {
					t.Fatal("the replaced memory is still live")
				}
			},
		},
		{
			name: "empty text forgets a learned memory", text: "",
			seed: func(t *testing.T, h *Host, sid string) string { return learned(t, h, "project", "deploy by hand") },
			want: " dropped",
			check: func(t *testing.T, h *Host, ui *fakeUI, replaces string) {
				live, _ := h.memoryList(false)
				if slices.ContainsFunc(live, func(m *store.Memory) bool { return m.ID == replaces }) {
					t.Fatal("not deleted")
				}
			},
		},
		{
			name: "empty text under ask proposes the forget", autonomy: "ask", text: "",
			seed: func(t *testing.T, h *Host, sid string) string { return learned(t, h, "project", "deploy by hand") },
			want: "queued for the user's review",
			check: func(t *testing.T, h *Host, ui *fakeUI, replaces string) {
				if p := h.memoryProposals(); len(p) != 1 || !p[0].Forget || p[0].Supersedes != replaces {
					t.Fatalf("proposals: %+v", p)
				}
			},
		},
		{
			name: "empty text on the user's memory asks", text: "", refused: true,
			seed: func(t *testing.T, h *Host, sid string) string {
				if err := h.Remember("deploy by hand"); err != nil {
					t.Fatal(err)
				}
				return projectMemories(t, h)[0].ID
			},
			want: "permission denied",
			check: func(t *testing.T, h *Host, ui *fakeUI, _ string) {
				if len(ui.asked) != 1 || ui.asked[0].Subject != "memory.write" || ui.asked[0].Preview != "deploy by hand" {
					t.Fatalf("asked: %+v", ui.asked)
				}
			},
		},
		{name: "empty text without replaces is an error", text: "", refused: true, want: "text is empty"},
		{
			name: "/memory keeps two facts that differ in one word", note: true, text: "the web server listens on port 8080 when run with make dev",
			seed: func(t *testing.T, h *Host, sid string) string {
				if err := h.Remember("the api server listens on port 8080 when run with make dev"); err != nil {
					t.Fatal(err)
				}
				return ""
			},
			check: func(t *testing.T, h *Host, ui *fakeUI, _ string) {
				if n := len(projectMemories(t, h)); n != 2 {
					t.Fatalf("stored %d", n)
				}
			},
		},
		{
			name: "/memory over budget cuts its oldest learned lines", note: true, text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string { return fill(t, h, 11) },
			check: func(t *testing.T, h *Host, ui *fakeUI, oldest string) {
				live, _ := h.memoryList(false)
				if slices.ContainsFunc(live, func(m *store.Memory) bool { return m.ID == oldest }) {
					t.Fatal("the note stored without a cut")
				}
			},
		},
		{
			name: "/memory refuses when nothing learned can make room", note: true, text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string {
				for i := range 11 {
					seedMemory(t, h, &store.Memory{Scope: "project", Text: filler(i), ProvenanceSessionID: "seed", Origin: store.OriginUser, UpdatedAt: int64(i + 1)})
				}
				return ""
			},
			refused: true, want: "project memory is full; forget one of its oldest in /memory first (its oldest line ",
			check: func(t *testing.T, h *Host, ui *fakeUI, _ string) {
				if n := len(projectMemories(t, h)); n != 11 {
					t.Fatalf("stored over budget: %d", n)
				}
			},
		},
		{
			name: "a learned write into a scope full of the user's lines lives in the trail", text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string {
				for i := range 11 {
					seedMemory(t, h, &store.Memory{Scope: "project", Text: filler(i), ProvenanceSessionID: "seed", Origin: store.OriginUser, UpdatedAt: int64(i + 1)})
				}
				return ""
			},
			want: "in the memory trail (the project memory is full)",
			check: func(t *testing.T, h *Host, ui *fakeUI, _ string) {
				live, _ := h.memoryList(false)
				if len(live) != 11 {
					t.Fatalf("the trail-only store changed the hot file: %d live", len(live))
				}
				if !slices.ContainsFunc(h.memoryTrail(live), func(l brain.Line) bool { return l.Text == "the deploy script lives in bin/deploy" }) {
					t.Fatal("the trail does not hold the stored line")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, r := newTestHostDB(t)
			ui := &fakeUI{}
			h.SetUIHooks(ui)
			h.Settings.SetQuiet("memory.autonomy", cmp.Or(tc.autonomy, "auto"))
			h.SetProjectTrusted(true)
			// the ask-everything policy a plugin's veto makes, for the
			// memory.write case that asks
			askEverything(t, r)
			pipe := h.PipelineFor()
			sid := seedSession(t, h)
			replaces := ""
			if tc.seed != nil {
				replaces = tc.seed(t, h, sid)
			}
			var msg string
			var err error
			if tc.note {
				err = h.Remember(tc.text)
			} else {
				msg, err = h.memoryTool(pipe, sid, tools.MemoryCall{Scope: cmp.Or(tc.scope, store.ScopeProject), Text: tc.text, Quote: tc.quote, Replaces: replaces})
			}
			if tc.refused != (err != nil) {
				t.Fatalf("refused = %v, err = %v", tc.refused, err)
			}
			if err != nil {
				msg = err.Error()
			}
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("answer %q (err %v), want %q", msg, err, tc.want)
			}
			if strings.HasSuffix(tc.want, "one of ") {
				if live, _ := h.memoryList(false); !strings.Contains(msg, live[len(live)-1].ID) {
					t.Fatalf("the oldest id is not named: %q", msg)
				}
			}
			if tc.check != nil {
				tc.check(t, h, ui, replaces)
			}
		})
	}
}

// TestMemoryDel: project code deletes through Lua as the user does, and an
// unknown id is nil, err.
func TestMemoryDel(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	seedSession(t, h)
	m := &store.Memory{Scope: "project", Text: "old note about the pipeline", ProvenanceSessionID: "seed", Origin: store.OriginLearned}
	seedMemory(t, h, m)
	err := r.ExecuteChunk(t.Context(), `assert(zemi.memory.del("`+m.ID+`"))`, "model.lua", lua.OriginProject)
	if err != nil {
		t.Fatalf("project code: %v", err)
	}
	if live, _ := h.memoryList(false); slices.ContainsFunc(live, func(x *store.Memory) bool { return x.ID == m.ID }) {
		t.Fatal("forget left the memory live")
	}
	runUser(t, r, `
		local ok, err = zemi.memory.del("nope")
		assert(ok == nil and err:find("no such memory"), tostring(err))
	`)
}

// covers §10.7: the session's prompt carries the memories it started with
// (TestPromptPinned keeps later ones out).
func TestBrainAtSessionStart(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()

	if sys := h.buildSystem(); strings.Contains(sys, "## Memory") {
		t.Fatal("memory lines must be absent before any memory")
	}

	if err := h.Remember("the deploy script lives in bin/deploy"); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if err := h.ensureSession("hello"); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}

	sys := h.buildSystem()
	if !strings.Contains(sys, "## Memory\nLines earlier sessions kept, as [id who age].") {
		t.Fatalf("memory section missing from the context message:\n%s", sys)
	}
	if !strings.Contains(sys, "## this project\n- [") || !strings.Contains(sys, " user] the deploy script lives in bin/deploy") {
		t.Fatalf("memory line missing:\n%s", sys)
	}

	if err := h.Remember("later note"); err != nil {
		t.Fatalf("remember2: %v", err)
	}
	if got, _ := h.brainFor(); !strings.Contains(got, "later note") {
		t.Fatalf("brainFor missed the new memory: %q", got)
	}
}

// TestMemoryListActions drives the /memory list's actions by id, and an
// "id=text" edit: each applies at once, and as many undos as actions
// applied bring the stores back to where they started, ages included.
func TestMemoryListActions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cmds []string
		want string // the stores after cmds; "" when a command fails and nothing moves
	}{
		{"keep a suggestion", []string{"accept pp1dp1dp"}, "?project:pp2ep2ep:deploy runs bin/deploy, ?project:pp4gp4gp:the build runs with make check, ?user:pp3fp3fp:forget:short replies, project:mm1am1am:user:1:deploy by hand, project:pp1dp1dp:learned:new:run make check first, user:mm2bm2bm:learned:2:short replies, user:mm3cm3cm:learned:3:the build runs with make check"},
		{"keep a replacement retires what it replaces", []string{"accept pp2ep2ep"}, "?project:pp1dp1dp:run make check first, ?project:pp4gp4gp:the build runs with make check, ?user:pp3fp3fp:forget:short replies, project:pp2ep2ep:learned:new:deploy runs bin/deploy, user:mm2bm2bm:learned:2:short replies, user:mm3cm3cm:learned:3:the build runs with make check"},
		{"keep a forget", []string{"accept pp3fp3fp"}, "?project:pp1dp1dp:run make check first, ?project:pp2ep2ep:deploy runs bin/deploy, ?project:pp4gp4gp:the build runs with make check, project:mm1am1am:user:1:deploy by hand, user:mm3cm3cm:learned:3:the build runs with make check"},
		{"drop a suggestion", []string{"reject pp1dp1dp"}, "?project:pp2ep2ep:deploy runs bin/deploy, ?project:pp4gp4gp:the build runs with make check, ?user:pp3fp3fp:forget:short replies, project:mm1am1am:user:1:deploy by hand, user:mm2bm2bm:learned:2:short replies, user:mm3cm3cm:learned:3:the build runs with make check"},
		{"forget a memory", []string{"forget mm1am1am"}, "?project:pp1dp1dp:run make check first, ?project:pp2ep2ep:deploy runs bin/deploy, ?project:pp4gp4gp:the build runs with make check, ?user:pp3fp3fp:forget:short replies, user:mm2bm2bm:learned:2:short replies, user:mm3cm3cm:learned:3:the build runs with make check"},
		{"move a memory everywhere keeps its id and age", []string{"move mm1am1am"}, "?project:pp1dp1dp:run make check first, ?project:pp2ep2ep:deploy runs bin/deploy, ?project:pp4gp4gp:the build runs with make check, ?user:pp3fp3fp:forget:short replies, user:mm1am1am:user:1:deploy by hand, user:mm2bm2bm:learned:2:short replies, user:mm3cm3cm:learned:3:the build runs with make check"},
		{"move a suggestion everywhere", []string{"move pp1dp1dp"}, "?project:pp2ep2ep:deploy runs bin/deploy, ?project:pp4gp4gp:the build runs with make check, ?user:pp1dp1dp:run make check first, ?user:pp3fp3fp:forget:short replies, project:mm1am1am:user:1:deploy by hand, user:mm2bm2bm:learned:2:short replies, user:mm3cm3cm:learned:3:the build runs with make check"},
		{"a suggestion the other scope holds stays", []string{"move pp4gp4gp"}, ""},
		{"a forget suggestion does not move", []string{"move pp3fp3fp"}, ""},
		{"edit a memory", []string{"mm2bm2bm=keep replies short"}, "?project:pp1dp1dp:run make check first, ?project:pp2ep2ep:deploy runs bin/deploy, ?project:pp4gp4gp:the build runs with make check, ?user:pp3fp3fp:forget:short replies, project:mm1am1am:user:1:deploy by hand, user:mm2bm2bm:user:new:keep replies short, user:mm3cm3cm:learned:3:the build runs with make check"},
		{"edit a replacement keeps it as yours", []string{"pp2ep2ep=deploy runs bin/deploy --prod"}, "?project:pp1dp1dp:run make check first, ?project:pp4gp4gp:the build runs with make check, ?user:pp3fp3fp:forget:short replies, project:pp2ep2ep:user:new:deploy runs bin/deploy --prod, user:mm2bm2bm:learned:2:short replies, user:mm3cm3cm:learned:3:the build runs with make check"},
		{"undo goes newest first, edits included", []string{"accept pp1dp1dp", "pp1dp1dp=run make check before a push", "reject pp2ep2ep", "move mm2bm2bm", "forget mm1am1am"}, "?project:pp4gp4gp:the build runs with make check, ?user:pp3fp3fp:forget:short replies, project:mm2bm2bm:learned:2:short replies, project:pp1dp1dp:user:new:run make check before a push, user:mm3cm3cm:learned:3:the build runs with make check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestHostDB(t)
			for n, m := range []*store.Memory{
				{ID: "mm1am1am", Scope: "project", Text: "deploy by hand", Origin: store.OriginUser},
				{ID: "mm2bm2bm", Scope: "user", Text: "short replies", Origin: store.OriginLearned},
				{ID: "mm3cm3cm", Scope: "user", Text: "the build runs with make check", Origin: store.OriginLearned},
			} {
				m.ProvenanceSessionID, m.CreatedAt, m.UpdatedAt = "seed", int64(n+1)*86400000, int64(n+1)*86400000
				seedMemory(t, h, m)
			}
			for _, p := range []memoryProposal{
				{ID: "pp1dp1dp", Scope: "project", Text: "run make check first"},
				{ID: "pp2ep2ep", Scope: "project", Text: "deploy runs bin/deploy", Supersedes: "mm1am1am"},
				{ID: "pp3fp3fp", Scope: "user", Text: "short replies", Supersedes: "mm2bm2bm", Forget: true},
				{ID: "pp4gp4gp", Scope: "project", Text: "the build runs with make check"},
			} {
				p.ProvenanceSessionID, p.Origin = "seed", store.OriginLearned
				if err := h.addProposal(p); err != nil {
					t.Fatal(err)
				}
			}
			stores := func() string {
				var out []string
				live, _ := h.memoryList(false)
				for _, m := range live {
					age := "new" // written by this test's commands
					if m.UpdatedAt < 1e10 {
						age = strconv.FormatInt(m.UpdatedAt/86400000, 10)
					}
					out = append(out, strings.Join([]string{string(m.Scope), m.ID, string(m.Origin), age, m.Text}, ":"))
				}
				for _, p := range h.memoryProposals() {
					out = append(out, "?"+string(p.Scope)+":"+p.ID+":"+map[bool]string{true: "forget:"}[p.Forget]+p.Text)
				}
				slices.Sort(out)
				return strings.Join(out, ", ")
			}
			start, applied := stores(), 0
			for _, c := range tc.cmds {
				var err error
				if id, text, ok := strings.Cut(c, "="); ok {
					_, err = h.rewriteMemory(id, text)
				} else {
					err = h.memoryCommand(c + " --yes")
				}
				if err == nil {
					applied++
				} else if tc.want != "" {
					t.Fatalf("%s: %v", c, err)
				}
			}
			if got := stores(); got != cmp.Or(tc.want, start) {
				t.Fatalf("after %v:\n got %s\nwant %s", tc.cmds, got, cmp.Or(tc.want, start))
			}
			if keys := h.memoryOverlay().Keys; len(keys) != min(applied, 1) {
				t.Fatalf("keys after %d actions: %+v", applied, keys)
			}
			for range applied {
				if err := h.memoryCommand("undo --yes"); err != nil {
					t.Fatal(err)
				}
			}
			if got := stores(); got != start {
				t.Fatalf("undone:\n got %s\nwant %s", got, start)
			}
			if err := h.memoryCommand("undo --yes"); !errors.Is(err, ErrUsage) || len(h.memoryOverlay().Keys) != 0 {
				t.Fatalf("an undo past the first action: %v", err)
			}
		})
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
		{"stored reworded", "mm1am1am", "deploy with make ship", "deploy with make ship"},
		{"stored unchanged", "mm1am1am", "deploy   by hand\n", ""},
		{"empty file", "mm1am1am", "\n", ""},
		{"secret blob refused", "mm1am1am", "AKIAIOSFODNN7EXAMPLE", ""},
		{"duplicate of another memory", "mm1am1am", "Other note", ""},
		{"suggestion reworded is kept as yours", "pp1dp1dp", "deploy with make ship", "deploy with make ship"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestHostDB(t)
			seedSession(t, h)
			for _, m := range []*store.Memory{
				{ID: "mm1am1am", Scope: "project", Text: "deploy by hand", ProvenanceSessionID: "seed", Origin: store.OriginLearned},
				{ID: "mm2bm2bm", Scope: "project", Text: "other note", ProvenanceSessionID: "seed", Origin: store.OriginLearned},
			} {
				seedMemory(t, h, m)
			}
			p := memoryProposal{ID: "pp1dp1dp", Scope: "project", Text: "suggested note", ProvenanceSessionID: "seed", Origin: store.OriginLearned}
			if err := h.addProposal(p); err != nil {
				t.Fatal(err)
			}
			ui := &fakeUI{edit: func(path string) { _ = os.WriteFile(path, []byte(tc.text), 0o600) }}
			h.editMemory(ui, tc.row)

			_, m, err := h.locateMemory(tc.row)
			switch {
			case tc.want == "" && tc.row == "mm1am1am":
				if m.Text != "deploy by hand" || m.Origin != store.OriginLearned {
					t.Fatalf("changed: %+v", m)
				}
			case err != nil || m.Text != tc.want || m.Origin != store.OriginUser:
				t.Fatalf("edited: %+v, %v", m, err)
			case tc.row == "pp1dp1dp" && len(h.memoryProposals()) != 0:
				t.Fatal("the edited suggestion is still queued")
			}
		})
	}
}

// TestMoveMergeKeepsUserOrigin: moving a user's memory onto a learned copy
// of the same text leaves that copy user-attested.
func TestMoveMergeKeepsUserOrigin(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	seedSession(t, h)
	if err := h.Remember("deploy by hand"); err != nil {
		t.Fatal(err)
	}
	seedMemory(t, h, &store.Memory{Scope: "user", Text: "Deploy by hand", ProvenanceSessionID: "seed", Origin: store.OriginLearned})
	if err := h.moveMemory(projectMemories(t, h)[0].ID, "project", "user"); err != nil {
		t.Fatal(err)
	}
	live, _ := h.memoryList(false)
	if len(live) != 1 || live[0].Origin != store.OriginUser || len(projectMemories(t, h)) != 0 {
		t.Fatalf("merge: %+v", live)
	}
}

func TestAcceptProposalKeepsItOnStoreError(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	// no origin: the store refuses the row, after the proposal was taken
	payload, _ := json.Marshal(memoryProposal{ID: "pp1dp1dp", Scope: "project", Text: "unstorable"}, json.Deterministic(true))
	if err := h.DB.AddProposal(&store.Proposal{ID: "pp1dp1dp", Payload: string(payload)}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.acceptProposal("pp1dp1dp"); err == nil {
		t.Fatal("accept stored a memory without origin")
	}
	if props := h.memoryProposals(); len(props) != 1 || props[0].ID != "pp1dp1dp" {
		t.Fatalf("proposal lost: %+v", props)
	}
}

// TestMemoryFileMode pins the file mode's gate (DESIGN §10.4): the
// frontmatter intake, the overwrite gate over the session write set, the
// write entry's restorable prior bytes, and the refusals.
func TestMemoryFileMode(t *testing.T) {
	const fm = "---\ndescription: repo map\nread_if: before code changes\n---\nbody\n"
	for _, tc := range []struct {
		name, path, content string
		autonomy            string
		seed                func(t *testing.T, h *Host)
		want                string
		check               func(t *testing.T, h *Host, sid string)
	}{
		{
			name: "a creation applies", path: "map.md", content: fm, want: "wrote map.md",
			check: func(t *testing.T, h *Host, sid string) {
				_, private := h.brainParts()
				b, err := os.ReadFile(filepath.Join(private.Dir, "map.md"))
				if err != nil || !strings.Contains(string(b), "repo map") {
					t.Fatalf("file: %q, %v", b, err)
				}
				lg, _ := private.OpenLedger()
				defer func() { _ = lg.Close() }()
				ops, _ := lg.Session(sid, 0, time.Now().UnixMilli()+1)
				if len(ops) != 1 || ops[0].Op != brain.OpWrite || ops[0].Existed || ops[0].Prior != nil {
					t.Fatalf("write op: %+v", ops)
				}
			},
		},
		{
			name: "the reserved name refuses", path: "memories.md", content: fm,
			want: "the memory tool owns this file",
		},
		{
			name: "outside the brain refuses", path: "../outside.md", content: fm,
			want: "the memory tool writes files in its private brain",
		},
		{
			name: "multi-line frontmatter queues under ask", path: "map.md", autonomy: "ask",
			content: "---\ndescription: |\n  two lines\nread_if: before code\n---\nbody", want: "queued for the user's review (multi-line frontmatter)",
		},
		{
			name: "oversized frontmatter queues under ask", path: "map.md", autonomy: "ask",
			content: "---\ndescription: " + strings.Repeat("d ", 150) + "\nread_if: before code\n---\nbody",
			want:    "queued for the user's review (the description or read_if is over 200 characters)",
		},
		{
			name: "instruction-shaped frontmatter queues under ask", path: "map.md", autonomy: "ask",
			content: "---\ndescription: always run make check first\nread_if: before code\n---\nbody",
			want:    "queued for the user's review (instruction-shaped frontmatter)",
		},
		{
			name: "instruction-shaped frontmatter applies under auto", path: "map.md",
			content: "---\ndescription: always run make check first\nread_if: before code\n---\nbody",
			want:    "wrote map.md",
		},
		{name: "missing frontmatter applies", path: "map.md", content: "just a body", want: "wrote map.md"},
		{
			name: "a secret in the content refuses", path: "map.md", content: fm + "token = ghp_0123456789abcdefghijklmnopqrstuv\n",
			want: "cannot hold a secret",
		},
		{
			name: "over the content cap refuses", path: "map.md", content: fm + strings.Repeat("w", 33<<10),
			want: "over the 32768-byte cap",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestHostDB(t)
			h.SetProjectTrusted(true)
			sid := seedSession(t, h)
			h.Settings.SetQuiet("memory.autonomy", cmp.Or(tc.autonomy, "auto"))
			if tc.seed != nil {
				tc.seed(t, h)
			}
			_, private := h.brainParts()
			if err := os.MkdirAll(private.Dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if db, err := store.Open(private.DB); err == nil {
				t.Cleanup(func() { _ = db.Close() })
			}
			p := filepath.Join(private.Dir, tc.path)
			msg, err := h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Path: p, Content: tc.content})
			if err != nil {
				msg = err.Error()
			}
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("answer %q, want %q", msg, tc.want)
			}
			if tc.check != nil {
				tc.check(t, h, sid)
			}
		})
	}

	t.Run("under ask the overwrite gate names a foreign file and a stale one", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHostDB(t)
		h.SetProjectTrusted(true)
		sid := seedSession(t, h)
		h.Settings.SetQuiet("memory.autonomy", "ask")
		_, private := h.brainParts()
		if err := os.MkdirAll(private.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if db, err := store.Open(private.DB); err == nil {
			t.Cleanup(func() { _ = db.Close() })
		}
		path := filepath.Join(private.Dir, "map.md")
		// a file this session did not write: the overwrite queues naming it
		if err := os.WriteFile(path, []byte("foreign"), 0o600); err != nil {
			t.Fatal(err)
		}
		if msg, err := h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Path: path, Content: fm}); err != nil || !strings.Contains(msg, "queued for the user's review (the file was not written by this session") {
			t.Fatalf("foreign overwrite: %q, %v", msg, err)
		}
		if err := h.acceptFileProposal(&h.memoryProposals()[0]); err != nil {
			t.Fatal(err)
		}
		// a foreign edit breaks the write set's hash: the next write queues
		if err := os.WriteFile(path, []byte(fm+"edited by a window\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		msg, err := h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Path: path, Content: fm})
		if err != nil || !strings.Contains(msg, "queued for the user's review (the file changed since this session wrote it") {
			t.Fatalf("stale overwrite: %q, %v", msg, err)
		}
		if err := h.acceptFileProposal(&h.memoryProposals()[0]); err != nil {
			t.Fatal(err)
		}
		// the accept joined the write set: the gate passes an unchanged file on
		if msg, err := h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Path: path, Content: fm}); err != nil || !strings.Contains(msg, "queued for the user's review (memory.autonomy is not auto)") {
			t.Fatalf("write after accept: %q, %v", msg, err)
		}
	})

	t.Run("the write entry keeps the prior bytes for rewind", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHostDB(t)
		h.SetProjectTrusted(true)
		sid := seedSession(t, h)
		h.Settings.SetQuiet("memory.autonomy", "auto")
		_, private := h.brainParts()
		if err := os.MkdirAll(private.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if db, err := store.Open(private.DB); err == nil {
			t.Cleanup(func() { _ = db.Close() })
		}
		path := filepath.Join(private.Dir, "map.md")
		const first = "---\ndescription: old\nread_if: old\n---\nold body\n"
		if _, err := h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Path: path, Content: first}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Path: path, Content: fm}); err != nil {
			t.Fatal(err)
		}
		lg, _ := private.OpenLedger()
		defer func() { _ = lg.Close() }()
		ops, _ := lg.Session(sid, 0, time.Now().UnixMilli()+1)
		if len(ops) != 2 || !ops[1].Existed || string(ops[1].Prior) != first || ops[1].Truncated {
			t.Fatalf("write op: %+v", ops)
		}
	})
}

// TestMemoryFileRefusesUserBrain pins the user partition's refusal (DESIGN §10.4).
func TestMemoryFileRefusesUserBrain(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)
	user, _ := h.brainParts()
	abs := filepath.Join(user.Dir, "style.md")
	msg, err := h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Path: abs, Content: "---\ndescription: d\nread_if: r\n---\nbody"})
	if err == nil || !strings.Contains(err.Error(), "user-brain files are written with the file tools") {
		t.Fatalf("user brain: %q, %v", msg, err)
	}
}

// TestPromoteMemory covers the memory tool's promote mode (DESIGN §10.4):
// the gate per origin and scope, the refusals, the accept/undo lifecycle and
// what a rewind replays.
func TestPromoteMemory(t *testing.T) {
	setup := func(t *testing.T) (*Host, string, brain.Partition) {
		t.Helper()
		h, _ := newTestHostDB(t)
		h.SetProjectTrusted(true)
		sid := seedSession(t, h)
		h.Settings.SetQuiet("memory.autonomy", "auto")
		_, private := h.brainParts()
		if err := os.MkdirAll(private.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if db, err := store.Open(private.DB); err == nil {
			t.Cleanup(func() { _ = db.Close() })
		}
		return h, sid, private
	}
	seedLine := func(t *testing.T, h *Host, scope store.Scope, origin store.Origin, text string) store.Memory {
		t.Helper()
		m := store.Memory{Scope: scope, Text: text, Origin: origin, ProvenanceSessionID: "seed", UpdatedAt: 1}
		seedMemory(t, h, &m)
		return m
	}
	call := func(h *Host, sid, id, name, description, readIf string) (string, error) {
		return h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Promote: id, Path: name, Description: description, ReadIf: readIf})
	}
	// fill a scope until the budget cuts its oldest learned line
	cutOldest := func(t *testing.T, h *Host, scope store.Scope) string {
		t.Helper()
		var archived string
		for i := range 12 {
			m := store.Memory{Scope: scope, Text: strings.TrimSpace(strings.Repeat(fmt.Sprintf("w%02d ", i), 65)), Origin: store.OriginLearned, ProvenanceSessionID: "seed", UpdatedAt: int64(i + 1)}
			if _, cut, err := brain.Apply(h.partition(scope), m, "", time.Now()); err != nil {
				t.Fatal(err)
			} else if len(cut) > 0 && archived == "" {
				archived = cut[0]
			}
		}
		if archived == "" {
			t.Fatal("the fill cut nothing")
		}
		return archived
	}

	for _, tc := range []struct {
		name                string
		autonomy            string
		seed                func(t *testing.T, h *Host) store.Memory
		path, want          string
		description, readIf string
		check               func(t *testing.T, h *Host, m store.Memory)
	}{
		{
			name: "a user line queues under ask", autonomy: "ask", path: "d.md", description: "d", readIf: "r",
			seed: func(t *testing.T, h *Host) store.Memory {
				return seedLine(t, h, "project", store.OriginUser, "deploy by hand")
			},
			want: "queued for the user's review (it moves a memory you saved)",
		},
		{
			name: "a user line applies under auto", path: "d.md", description: "d", readIf: "r",
			seed: func(t *testing.T, h *Host) store.Memory {
				return seedLine(t, h, "project", store.OriginUser, "deploy by hand")
			},
			want: "promoted ",
		},
		{
			name: "a learned line in the user scope queues under ask", autonomy: "ask", path: "d.md", description: "d", readIf: "r",
			seed: func(t *testing.T, h *Host) store.Memory {
				return seedLine(t, h, "user", store.OriginLearned, "short replies everywhere")
			},
			want: "(it writes into the brain every project sees)",
		},
		{
			name: "instruction-shaped frontmatter queues under ask", autonomy: "ask", path: "d.md", description: "always run make check first", readIf: "r",
			seed: func(t *testing.T, h *Host) store.Memory {
				return seedLine(t, h, "project", store.OriginLearned, "a plain fact")
			},
			want: "(instruction-shaped frontmatter)",
		},
		{
			name: "missing read_if refuses", path: "d.md", description: "d", readIf: "",
			seed: func(t *testing.T, h *Host) store.Memory {
				return seedLine(t, h, "project", store.OriginLearned, "a plain fact")
			},
			want: "needs description and read_if",
		},
		{
			name: "a secret in the frontmatter refuses", path: "d.md", description: "the token is ghp_0123456789abcdefghijklmnopqrstuv", readIf: "r",
			seed: func(t *testing.T, h *Host) store.Memory {
				return seedLine(t, h, "project", store.OriginLearned, "a plain fact")
			},
			want: "cannot hold a secret",
		},
		{
			name: "a collision refuses", path: "f00.md", description: "d", readIf: "r",
			seed: func(t *testing.T, h *Host) store.Memory {
				_, private := h.brainParts()
				if err := os.MkdirAll(private.Dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(private.Dir, "f00.md"), []byte("taken"), 0o600); err != nil {
					t.Fatal(err)
				}
				return seedLine(t, h, "project", store.OriginLearned, "a plain fact")
			},
			want: "already exists",
		},
		{
			name: "a full catalog refuses", path: "zz.md", description: "d", readIf: "r",
			seed: func(t *testing.T, h *Host) store.Memory {
				_, private := h.brainParts()
				if err := os.MkdirAll(private.Dir, 0o700); err != nil {
					t.Fatal(err)
				}
				for i := range brain.MaxCatalogFiles {
					f := filepath.Join(private.Dir, fmt.Sprintf("f%02d.md", i))
					if err := os.WriteFile(f, brain.ComposeFile("d", "r", "b"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return seedLine(t, h, "project", store.OriginLearned, "a plain fact")
			},
			want: "catalog is full",
		},
		{
			name: "a forgotten id refuses", path: "d.md", description: "d", readIf: "r",
			seed: func(t *testing.T, h *Host) store.Memory {
				m := seedLine(t, h, "project", store.OriginLearned, "a forgotten fact")
				if err := brain.Forget(h.partition("project"), "seed", m.ID, time.Now()); err != nil {
					t.Fatal(err)
				}
				return m
			},
			want: "is forgotten; save it fresh instead",
		},
		{
			name: "learned in private under auto applies", path: "d.md", description: "the deploy map", readIf: "before any deploy",
			seed: func(t *testing.T, h *Host) store.Memory {
				return seedLine(t, h, "project", store.OriginLearned, "the deploy script lives in bin/deploy")
			},
			want: "promoted ",
			check: func(t *testing.T, h *Host, m store.Memory) {
				_, private := h.brainParts()
				b, err := os.ReadFile(filepath.Join(private.Dir, "d.md"))
				if err != nil || !strings.Contains(string(b), "the deploy script lives in bin/deploy") {
					t.Fatalf("file = %q, %v", b, err)
				}
				if fm, err := brain.ParseFrontmatter(string(b)); err != nil || fm.Description != "the deploy map" {
					t.Fatalf("frontmatter = %+v, %v", fm, err)
				}
				if lines, _ := brain.Lines(private); slices.ContainsFunc(lines, func(l brain.Line) bool { return l.ID == m.ID }) {
					t.Fatal("the line stayed live")
				}
				l := brain.Load(private)
				if !slices.ContainsFunc(l.Entries, func(e brain.Entry) bool { return e.Name == "d.md" }) {
					t.Fatal("the catalog does not name the file")
				}
				if hash, ours := h.brainWriteOf(filepath.Join(private.Dir, "d.md")); !ours || hash == "" {
					t.Fatal("the write set does not name the file")
				}
			},
		},
		{
			name: "a ledger-only id promotes with no line change", path: "old.md", description: "an archived fact", readIf: "r",
			seed: func(t *testing.T, h *Host) store.Memory {
				return store.Memory{ID: cutOldest(t, h, "project"), Scope: "project", Origin: store.OriginLearned}
			},
			want: "promoted ",
			check: func(t *testing.T, h *Host, m store.Memory) {
				live, _ := h.memoryList(false)
				if slices.ContainsFunc(live, func(x *store.Memory) bool { return x.ID == m.ID }) {
					t.Fatal("the archived line came back live")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, sid, _ := setup(t)
			h.Settings.SetQuiet("memory.autonomy", cmp.Or(tc.autonomy, "auto"))
			m := tc.seed(t, h)
			msg, err := call(h, sid, m.ID, tc.path, tc.description, tc.readIf)
			if err != nil {
				msg = err.Error()
			}
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("answer %q, want %q", msg, tc.want)
			}
			if tc.check != nil {
				tc.check(t, h, m)
			}
		})
	}

	t.Run("accept writes and undo restores the line and the file", func(t *testing.T) {
		t.Parallel()
		h, _, _ := setup(t)
		h.Settings.SetQuiet("memory.autonomy", "ask")
		m := seedLine(t, h, "project", store.OriginUser, "deploy by hand")
		if _, err := call(h, "sess0001", m.ID, "out.md", "the deploy map", "r"); err != nil {
			t.Fatal(err)
		}
		p := h.memoryProposals()[0]
		if err := h.memoryCommand("accept " + p.ID + " --yes"); err != nil {
			t.Fatal(err)
		}
		_, user := h.brainParts()
		b, err := os.ReadFile(filepath.Join(user.Dir, "out.md"))
		if err != nil || !strings.Contains(string(b), "deploy by hand") {
			t.Fatalf("file = %q, %v", b, err)
		}
		if lines, _ := brain.Lines(user); slices.ContainsFunc(lines, func(l brain.Line) bool { return l.ID == m.ID }) {
			t.Fatal("the line stayed live")
		}
		if err := h.memoryCommand("undo --yes"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(user.Dir, "out.md")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the file survived the undo: %v", err)
		}
		live, _ := h.memoryList(false)
		if !slices.ContainsFunc(live, func(x *store.Memory) bool { return x.ID == m.ID && x.Text == "deploy by hand" }) {
			t.Fatal("the undo did not restore the line")
		}
	})

	t.Run("an edited file survives the undo, the line comes back", func(t *testing.T) {
		t.Parallel()
		h, _, _ := setup(t)
		h.Settings.SetQuiet("memory.autonomy", "ask")
		m := seedLine(t, h, "project", store.OriginLearned, "a plain fact")
		if _, err := call(h, "sess0001", m.ID, "out.md", "d", "r"); err != nil {
			t.Fatal(err)
		}
		if err := h.memoryCommand("accept " + h.memoryProposals()[0].ID + " --yes"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h.partition("project").Dir, "out.md"), []byte("edited by hand\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := h.memoryCommand("undo --yes"); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(h.partition("project").Dir, "out.md"))
		if err != nil || string(b) != "edited by hand\n" {
			t.Fatalf("file = %q, %v", b, err)
		}
		live, _ := h.memoryList(false)
		if !slices.ContainsFunc(live, func(x *store.Memory) bool { return x.ID == m.ID }) {
			t.Fatal("the undo did not restore the line")
		}
	})

	t.Run("a replace between accept and undo makes the undo a no-op", func(t *testing.T) {
		t.Parallel()
		h, _, _ := setup(t)
		h.Settings.SetQuiet("memory.autonomy", "ask")
		m := seedLine(t, h, "project", store.OriginLearned, "a plain fact")
		if _, err := call(h, "sess0001", m.ID, "out.md", "d", "r"); err != nil {
			t.Fatal(err)
		}
		if err := h.memoryCommand("accept " + h.memoryProposals()[0].ID + " --yes"); err != nil {
			t.Fatal(err)
		}
		// the replace supersedes the promoted id with a text-less entry
		if _, err := h.Settings.SetQuiet("memory.autonomy", "auto"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.memoryTool(h.PipelineFor(), "sess0001", tools.MemoryCall{Scope: "project", Text: "a plain fact, kept", Replaces: m.ID}); err != nil {
			t.Fatal(err)
		}
		if err := h.memoryCommand("undo --yes"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(h.partition("project").Dir, "out.md")); err != nil {
			t.Fatalf("the replacement's file vanished: %v", err)
		}
		live, _ := h.memoryList(false)
		if !slices.ContainsFunc(live, func(x *store.Memory) bool { return x.Text == "a plain fact, kept" }) {
			t.Fatal("the undo disturbed the replacement")
		}
		if slices.ContainsFunc(live, func(x *store.Memory) bool { return x.ID == m.ID }) {
			t.Fatal("the undo restored the replaced line")
		}
	})

	t.Run("a replace of a promoted id proceeds and names the file", func(t *testing.T) {
		t.Parallel()
		h, _, _ := setup(t)
		m := seedLine(t, h, "project", store.OriginLearned, "a plain fact")
		msg, err := call(h, "sess0001", m.ID, "out.md", "d", "r")
		if err != nil {
			t.Fatal(err)
		}
		out, err := h.memoryTool(h.PipelineFor(), "sess0001", tools.MemoryCall{Scope: "project", Text: "a plain fact, restated", Replaces: m.ID})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "its text now lives in out.md") {
			t.Fatalf("replace = %q (promote = %q)", out, msg)
		}
	})

	t.Run("a name promoted in both partitions rewinds both files", func(t *testing.T) {
		t.Parallel()
		h, sid, private := setup(t)
		private1 := seedLine(t, h, "project", store.OriginLearned, "a private fact")
		if _, err := call(h, sid, private1.ID, "same.md", "d", "r"); err != nil {
			t.Fatal(err)
		}
		h.Settings.SetQuiet("memory.autonomy", "ask")
		user1 := seedLine(t, h, "user", store.OriginLearned, "a user fact")
		if _, err := call(h, sid, user1.ID, "same.md", "d", "r"); err != nil {
			t.Fatal(err)
		}
		if err := h.memoryCommand("accept " + h.memoryProposals()[0].ID + " --yes"); err != nil {
			t.Fatal(err)
		}
		_, user := h.brainParts()
		if _, err := os.Stat(filepath.Join(private.Dir, "same.md")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(user.Dir, "same.md")); err != nil {
			t.Fatal(err)
		}
		h.forgetLearned(sid, 0, time.Now().UnixMilli()+1, nil)
		for _, p := range []brain.Partition{private, user} {
			if _, err := os.Stat(filepath.Join(p.Dir, "same.md")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the rewind left %s", filepath.Join(p.Dir, "same.md"))
			}
		}
	})

	t.Run("rewind restores the line and deletes the file", func(t *testing.T) {
		t.Parallel()
		h, sid, private := setup(t)
		m := seedLine(t, h, "project", store.OriginLearned, "a plain fact")
		if _, err := call(h, sid, m.ID, "out.md", "d", "r"); err != nil {
			t.Fatal(err)
		}
		h.forgetLearned(sid, 0, time.Now().UnixMilli()+1, nil)
		if _, err := os.Stat(filepath.Join(private.Dir, "out.md")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the file survived the rewind: %v", err)
		}
		live, _ := h.memoryList(false)
		if !slices.ContainsFunc(live, func(x *store.Memory) bool { return x.ID == m.ID && x.Text == "a plain fact" }) {
			t.Fatal("the rewind did not restore the line")
		}
	})

	t.Run("a ledger-only promote undone in its session stays ledger-only under rewind", func(t *testing.T) {
		t.Parallel()
		h, sid, private := setup(t)
		h.Settings.SetQuiet("memory.autonomy", "ask")
		archived := cutOldest(t, h, "project")
		if _, err := call(h, sid, archived, "old.md", "an archived fact", "r"); err != nil {
			t.Fatal(err)
		}
		if err := h.memoryCommand("accept " + h.memoryProposals()[0].ID + " --yes"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(private.Dir, "old.md")); err != nil {
			t.Fatal(err)
		}
		if err := h.memoryCommand("undo --yes"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(private.Dir, "old.md")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("the undo left the file")
		}
		h.forgetLearned(sid, 0, time.Now().UnixMilli()+1, nil)
		live, _ := h.memoryList(false)
		if slices.ContainsFunc(live, func(x *store.Memory) bool { return x.ID == archived }) {
			t.Fatal("the rewind resurrected the line live")
		}
		if _, err := os.Stat(filepath.Join(private.Dir, "old.md")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("the rewind left the file")
		}
		lg, _ := private.OpenLedger()
		defer func() { _ = lg.Close() }()
		trail, err := lg.ByID(archived)
		if err != nil {
			t.Fatal(err)
		}
		if last := trail[len(trail)-1]; last.Op != brain.OpArchive || last.Text == "" || last.SessionID != "user" {
			t.Fatalf("trail end = %+v", last)
		}
	})
}

// TestMemorySearch covers the memory tool's query mode (DESIGN §10.4).
func TestMemorySearch(t *testing.T) {
	setup := func(t *testing.T) (*Host, string, brain.Partition) {
		t.Helper()
		h, _ := newTestHostDB(t)
		h.SetProjectTrusted(true)
		sid := seedSession(t, h)
		h.Settings.SetQuiet("memory.autonomy", "auto")
		_, private := h.brainParts()
		if err := os.MkdirAll(private.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if db, err := store.Open(private.DB); err == nil {
			t.Cleanup(func() { _ = db.Close() })
		}
		return h, sid, private
	}
	seedLine := func(t *testing.T, h *Host, scope store.Scope, origin store.Origin, text string) store.Memory {
		t.Helper()
		m := store.Memory{Scope: scope, Text: text, Origin: origin, ProvenanceSessionID: "seed", UpdatedAt: 1}
		seedMemory(t, h, &m)
		return m
	}
	search := func(t *testing.T, h *Host, q string) string {
		t.Helper()
		out, err := h.memoryTool(h.PipelineFor(), "sess0001", tools.MemoryCall{Query: q})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	t.Run("a live hit and an archived hit, by text and by id", func(t *testing.T) {
		t.Parallel()
		h, _, private := setup(t)
		seedLine(t, h, "project", store.OriginLearned, "the deploy script lives in bin/deploy")
		archived := ""
		for i := range 12 {
			m := store.Memory{Scope: "project", Text: strings.TrimSpace(strings.Repeat(fmt.Sprintf("w%02d ", i), 65)), Origin: store.OriginLearned, ProvenanceSessionID: "seed", UpdatedAt: int64(i + 1)}
			if _, cut, err := brain.Apply(private, m, "", time.Now()); err != nil {
				t.Fatal(err)
			} else if len(cut) > 0 && archived == "" {
				archived = cut[0]
			}
		}
		if archived == "" {
			t.Fatal("the fill cut nothing")
		}
		// the first-applied line is the one the cut archived
		if got := search(t, h, "deploy"); !strings.Contains(got, "the deploy script lives in bin/deploy") {
			t.Fatalf("the archived text missed: %q", got)
		}
		if got := search(t, h, archived); !strings.Contains(got, archived) || !strings.Contains(got, "re-enter it") {
			t.Fatalf("the archived id missed: %q", got)
		}
	})

	t.Run("a forgotten id and a frozen row stay out", func(t *testing.T) {
		t.Parallel()
		h, _, private := setup(t)
		m := seedLine(t, h, "project", store.OriginLearned, "a forgotten fact")
		if err := brain.Forget(private, "seed", m.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
		seedFrozen(t, private, &store.Memory{ID: "fz1", Scope: "project", Text: "a frozen fact", Origin: store.OriginLearned, SupersededBy: "frozenid1", UpdatedAt: 1})
		got := search(t, h, "forgotten frozen")
		for _, want := range []string{"a forgotten fact", "a frozen fact"} {
			if strings.Contains(got, want) {
				t.Fatalf("search surfaced %q:\n%s", want, got)
			}
		}
	})

	t.Run("a drifted add recalls and the trail carries it", func(t *testing.T) {
		t.Parallel()
		h, _, private := setup(t)
		m := seedLine(t, h, "project", store.OriginLearned, "a memory the file lost")
		// the add reached the ledger, the file replace did not: the drift Lint reports
		if err := os.WriteFile(filepath.Join(private.Dir, brain.MemFile), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if got := search(t, h, "file lost"); !strings.Contains(got, m.ID) {
			t.Fatalf("the drifted add missed: %q", got)
		}
		live, _ := h.memoryList(false)
		if !slices.ContainsFunc(h.memoryTrail(live), func(l brain.Line) bool { return l.ID == m.ID }) {
			t.Fatal("the trail does not hold the drifted add")
		}
	})

	t.Run("the cap holds and a hit re-enters through replaces", func(t *testing.T) {
		t.Parallel()
		h, _, private := setup(t)
		var archived string
		for i := range 12 {
			m := store.Memory{Scope: "project", Text: strings.TrimSpace(strings.Repeat(fmt.Sprintf("deploys filler %d ", i), 22)), Origin: store.OriginLearned, ProvenanceSessionID: "seed", UpdatedAt: int64(i + 1)}
			if _, cut, err := brain.Apply(private, m, "", time.Now()); err != nil {
				t.Fatal(err)
			} else if len(cut) > 0 && archived == "" {
				archived = cut[0]
			}
		}
		if archived == "" {
			t.Fatal("the fill cut nothing")
		}
		got := search(t, h, "deploys")
		if !strings.Contains(got, "8 of 12 memories match") || !strings.Contains(got, "... 4 more matched") {
			t.Fatalf("search = %q", got)
		}
		// the archived hit recalls by its id, the one key cutNote hands out
		if byID := search(t, h, archived); !strings.Contains(byID, archived) {
			t.Fatalf("search by id = %q", byID)
		}
		// the cut hit re-enters: the near-duplicate scan covers live rows only
		out, err := h.memoryTool(h.PipelineFor(), "sess0001", tools.MemoryCall{Scope: "project", Text: "the deploys note, restated", Replaces: archived})
		if err != nil || !strings.Contains(out, "saved as ") {
			t.Fatalf("re-entry = %q, %v", out, err)
		}
	})

	t.Run("an empty query refuses", func(t *testing.T) {
		t.Parallel()
		h, _, _ := setup(t)
		if _, err := h.memoryTool(h.PipelineFor(), "sess0001", tools.MemoryCall{Query: "   "}); err == nil {
			t.Fatal("an empty query must fail")
		}
	})
}

func TestMemoryCommandsRegistered(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	if err := h.BuiltinCommandsStep().Run(t.Context(), h.RT, &lua.Generation{ID: 1}); err != nil {
		t.Fatalf("step: %v", err)
	}
	if h.Commands.Lookup("memory") == nil {
		t.Fatal("memory commands not registered")
	}
	if err := h.memoryCommand("forget"); err == nil {
		t.Fatal("forget without an id must fail")
	}
	if err := h.Remember("deploy by hand"); err != nil {
		t.Fatal(err)
	}
	// typed, a forget asks first: headless it names the --yes form, and with
	// a screen /memory opens on that memory
	id := projectMemories(t, h)[0].ID
	if err := h.memoryCommand("forget " + id); err == nil || !strings.Contains(err.Error(), "/memory forget "+id+" --yes runs it without asking") {
		t.Fatalf("a headless forget: %v", err)
	}
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.memoryCommand("forget " + id); err != nil {
		t.Fatal(err)
	}
	confirm := "memory forget " + id + " --yes"
	if o := ui.overlays(); len(o) != 1 || o[0].Confirm != confirm || !strings.Contains(fmt.Sprint(o[0].Items), confirm) || len(projectMemories(t, h)) != 1 {
		t.Fatalf("a typed forget: %+v, %d memories", o, len(projectMemories(t, h)))
	}
	if err := h.memoryCommand("forget " + id + " --yes"); err != nil {
		t.Fatal(err)
	}
	if n := h.evlog.notes[len(h.evlog.notes)-1]; n.title != "Memory forgotten" || n.body != "deploy by hand" {
		t.Fatalf("toast: %+v", n)
	}
	// opening the list starts with nothing to undo
	if err := h.memoryCommand(""); err != nil {
		t.Fatal(err)
	}
	if err := h.memoryCommand("undo --yes"); !errors.Is(err, ErrUsage) {
		t.Fatalf("an undo reached past the list's opening: %v", err)
	}
	// a row another window took is a usage error; a note may start with a verb
	if err := h.memoryCommand("accept " + id + " --yes"); !errors.Is(err, ErrUsage) {
		t.Fatalf("a gone row: %v", err)
	}
	if err := h.memoryCommand("edit tests first"); err != nil || projectMemories(t, h)[0].Text != "edit tests first" {
		t.Fatalf("a note starting with a verb: %v", err)
	}
}
