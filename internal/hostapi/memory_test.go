package hostapi

import (
	"bytes"
	"cmp"
	"encoding/json/v2"
	"errors"
	"fmt"
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

// TestMemoryAddAutonomyGate drives zemi.memory.add: ask queues, a
// near-duplicate names the row it repeats, user config before a session
// writes the user's own memory, and auto stores a plain fact with its id.
func TestMemoryAddAutonomyGate(t *testing.T) {
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
		closed      bool   // written by a session that is not the live one
		want        string // in the answer or the error
		check       func(t *testing.T, h *Host, ui *fakeUI, replaces string)
	}{
		{name: "project fact stores", text: "the deploy script lives in bin/deploy", want: "saved as "},
		{name: "ask queues", autonomy: "ask", text: "the deploy script lives in bin/deploy", want: "queued for the user's review"},
		{name: "instruction shape queues", text: "always run make check before a commit", want: "(instruction-shaped text)"},
		{
			name: "spent auto budget queues", text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string { h.agents.memAuto = memoryAutoApplyCap; return "" },
			want: "(this session's auto budget is spent)",
		},
		{
			name: "a closed session skips the live budget", closed: true, text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string { h.agents.memAuto = memoryAutoApplyCap; return "" },
			want: "saved as ",
		},
		{name: "user fact without a quote queues", scope: "user", text: "the user wants short replies", want: "(a memory about the user needs a quote)"},
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
			quote: "keep replies short", want: "dropped: quote not found",
		},
		{
			name: "quote in a compaction summary drops", scope: "user", text: "the user wants short replies",
			seed: func(t *testing.T, h *Host, sid string) string {
				addText(t, h, sid, "user", compactPrefix+"the user asked to keep replies short"+compactSuffix)
				return ""
			},
			quote: "keep replies short", want: "dropped: quote not found",
		},
		{
			name: "quote only in an attachment drops", scope: "user", text: "the user wants short replies",
			seed: func(t *testing.T, h *Host, sid string) string {
				addText(t, h, sid, "user", "read @notes.md"+agent.AttachMarker+"\nkeep replies short\n</attached-files>")
				return ""
			},
			quote: "keep replies short", want: "dropped: quote not found",
		},
		{
			name: "near-duplicate of the other scope", text: "make golden regenerates the UI golden files",
			seed: func(t *testing.T, h *Host, sid string) string {
				learned(t, h, "user", "the ui golden files are regenerated by make golden")
				return ""
			},
			want: `(user): "the ui golden files are regenerated by make golden"; to keep both facts, pass replaces=`,
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
			want: "already queued for the user's review as ",
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
			name: "an overflow that would cut a user line queues", text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string {
				m := &store.Memory{Scope: "project", Text: filler(0), ProvenanceSessionID: "seed", Origin: store.OriginUser, UpdatedAt: 1}
				seedMemory(t, h, m)
				for i := 1; i < 11; i++ {
					seedMemory(t, h, &store.Memory{Scope: "project", Text: filler(i), ProvenanceSessionID: "seed", Origin: store.OriginLearned, UpdatedAt: int64(i + 1)})
				}
				return ""
			},
			want: "queued for the user's review (project memory is full",
			check: func(t *testing.T, h *Host, ui *fakeUI, _ string) {
				live, _ := h.memoryList(false)
				if len(live) != 11 || !slices.ContainsFunc(live, func(m *store.Memory) bool { return m.Origin == store.OriginUser }) {
					t.Fatalf("the refused write cut something: %d live", len(live))
				}
				p := h.memoryProposals()
				if len(p) != 1 || p[0].Text != "the deploy script lives in bin/deploy" {
					t.Fatalf("the queued write left no proposal: %+v", p)
				}
			},
		},
		{
			name: "accepting into a scope whose cut would take a user line puts it back", autonomy: "ask", text: "the deploy script lives in bin/deploy",
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
				if _, err := h.acceptProposal(p[0].ID); err == nil || !strings.Contains(err.Error(), "project memory is full") {
					t.Fatalf("accept: %v", err)
				}
				if len(h.memoryProposals()) != 1 {
					t.Fatal("the proposal was not put back")
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
			name: "replacing the user's memory queues", text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string {
				if err := h.Remember("deploy by hand"); err != nil {
					t.Fatal(err)
				}
				return projectMemories(t, h)[0].ID
			},
			want: "(replaces a memory the user saved",
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
			name: "empty text on the user's memory asks", text: "",
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
		{name: "empty text without replaces is an error", text: "", want: "text is empty"},
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
			name: "/memory refuses when the cut would take a user line", note: true, text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string {
				m := &store.Memory{Scope: "project", Text: filler(0), ProvenanceSessionID: "seed", Origin: store.OriginUser, UpdatedAt: 1}
				seedMemory(t, h, m)
				for i := 1; i < 11; i++ {
					seedMemory(t, h, &store.Memory{Scope: "project", Text: filler(i), ProvenanceSessionID: "seed", Origin: store.OriginLearned, UpdatedAt: int64(i + 1)})
				}
				return m.ID
			},
			want: "project memory is full; forget one of its oldest in /memory first (its oldest line ",
			check: func(t *testing.T, h *Host, ui *fakeUI, oldest string) {
				if n := len(projectMemories(t, h)); n != 11 {
					t.Fatalf("stored over budget: %d", n)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			ui := &fakeUI{}
			h.SetUIHooks(ui)
			h.Settings.SetQuiet("memory.autonomy", cmp.Or(tc.autonomy, "auto"))
			h.SetProjectTrusted(true)
			sid := seedSession(t, h)
			replaces := ""
			if tc.seed != nil {
				replaces = tc.seed(t, h, sid)
			}
			if tc.closed {
				sid = seedSession(t, h) // the run's own session; another one is live now
				seedSession(t, h)
			}
			var msg string
			var err error
			if tc.note {
				err = h.Remember(tc.text)
			} else {
				msg, err = h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Scope: cmp.Or(tc.scope, store.ScopeProject), Text: tc.text, Quote: tc.quote, Replaces: replaces})
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

// TestMemoryAutoQueuesOutsideText: auto autonomy still queues in an
// untrusted project and in a session that it or a subagent fed text from
// outside the machine.
func TestMemoryAutoQueuesOutsideText(t *testing.T) {
	for _, tc := range []struct {
		name         string
		untrusted    bool
		tool, inTool string // a tool the session called, one its subagent called
		stored, fork bool
	}{
		{name: "trusted, local tools", tool: "shell", inTool: "read", stored: true},
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
			h.Settings.SetQuiet("memory.autonomy", "auto")
			h.SetProjectTrusted(!tc.untrusted)
			sid := seedSession(t, h)
			child := store.ShortID()
			if err := h.DB.CreateSession(child, sid, "sub", "web"); err != nil {
				t.Fatal(err)
			}
			if tc.tool != "" {
				h.recordToolCall(sid, StreamTool{ID: "c1", Name: tc.tool}, true)
			}
			if tc.inTool != "" {
				h.recordToolCall(child, StreamTool{ID: "c2", Name: tc.inTool}, true)
			}
			if tc.fork {
				if _, err := h.ForkSession(); err != nil {
					t.Fatal(err)
				}
			}
			out, err := h.proposeMemory(h.AgentSessionID(), store.OriginLearned, "project", "the build uses make", "", "")
			if err != nil || (out.ID != "") != tc.stored {
				t.Fatalf("stored = %+v, want %v (%v)", out, tc.stored, err)
			}
		})
	}
}

// TestMemoryDelOriginGate pins the review guard: model and plugin
// origins may never delete through Lua; user origin may.
func TestMemoryDelOriginGate(t *testing.T) {
	h, r := newTestHostDB(t)
	seedSession(t, h)
	m := &store.Memory{Scope: "project", Text: "old note about the pipeline", ProvenanceSessionID: "seed", Origin: store.OriginLearned}
	seedMemory(t, h, m)
	err := r.ExecuteChunk(t.Context(), `zemi.memory.del("`+m.ID+`")`, "model.lua", lua.OriginProject)
	if err == nil || !strings.Contains(err.Error(), "only user config") {
		t.Fatalf("model origin allowed: %v", err)
	}
	runUser(t, r, `
		local ok, err = zemi.memory.del("nope")
		assert(ok == nil and err:find("no such memory"), tostring(err))
		assert(zemi.memory.del("`+m.ID+`"))
	`)
	if live, _ := h.memoryList(false); slices.ContainsFunc(live, func(x *store.Memory) bool { return x.ID == m.ID }) {
		t.Fatal("forget left the memory live")
	}
}

// covers §10.7: the session's prompt carries the memories it started with
// (TestPromptPinned keeps later ones out).
func TestBrainAtSessionStart(t *testing.T) {
	h, _ := newTestHostDB(t)
	h.Dirs.DataHome = t.TempDir()

	if sys := h.buildSystem(nil); strings.Contains(sys, "## Memory") {
		t.Fatal("memory lines must be absent before any memory")
	}

	if err := h.Remember("the deploy script lives in bin/deploy"); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if err := h.ensureSession(nil, "hello"); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}

	sys := h.buildSystem(nil)
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
		seed                func(t *testing.T, h *Host)
		want                string
		check               func(t *testing.T, h *Host, sid string)
	}{
		{
			name: "a creation applies", path: "map.md", content: fm, want: "wrote map.md",
			check: func(t *testing.T, h *Host, sid string) {
				if h.agents.memAuto != 1 {
					t.Fatalf("the apply spent no auto budget: %d", h.agents.memAuto)
				}
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
			name: "the spent auto budget queues", path: "map.md", content: fm,
			seed: func(t *testing.T, h *Host) { h.agents.memAuto = memoryAutoApplyCap },
			want: "queued for the user's review (this session's auto budget is spent)",
		},
		{
			name: "an overwrite of a foreign file queues", path: "map.md", content: fm,
			seed: func(t *testing.T, h *Host) {
				_, private := h.brainParts()
				if err := os.MkdirAll(private.Dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(private.Dir, "map.md"), []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "queued for the user's review (the file was not written by this session",
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
			name: "multi-line frontmatter queues", path: "map.md",
			content: "---\ndescription: |\n  two lines\nread_if: before code\n---\nbody", want: "queued for the user's review (multi-line frontmatter)",
		},
		{
			name: "oversized frontmatter queues", path: "map.md",
			content: "---\ndescription: " + strings.Repeat("d ", 150) + "\nread_if: before code\n---\nbody",
			want:    "queued for the user's review (the description or read_if is over 200 characters)",
		},
		{
			name: "instruction-shaped frontmatter queues", path: "map.md",
			content: "---\ndescription: always run make check first\nread_if: before code\n---\nbody",
			want:    "queued for the user's review (instruction-shaped frontmatter)",
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
			h, _ := newTestHostDB(t)
			h.SetProjectTrusted(true)
			sid := seedSession(t, h)
			h.Settings.SetQuiet("memory.autonomy", "auto")
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

	t.Run("the overwrite gate queues a foreign file and a stale one", func(t *testing.T) {
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
		if _, err := h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Path: path, Content: fm}); err != nil {
			t.Fatal(err)
		}
		// the session's own write, unchanged, applies again
		if msg, err := h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Path: path, Content: fm + "more\n"}); err != nil || !strings.Contains(msg, "wrote map.md") {
			t.Fatalf("own overwrite: %q, %v", msg, err)
		}
		// a foreign edit breaks the write set's hash: the next write queues
		if err := os.WriteFile(path, []byte(fm+"edited by a window\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		msg, err := h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Path: path, Content: fm})
		if err != nil || !strings.Contains(msg, "queued for the user's review (the file changed since this session wrote it") {
			t.Fatalf("stale overwrite: %q, %v", msg, err)
		} // the queued proposal's accept writes the file and joins the write set
		p := h.memoryProposals()[0]
		if err := h.acceptFileProposal(&p); err != nil {
			t.Fatal(err)
		}
		if msg, err := h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Path: path, Content: fm}); err != nil || !strings.Contains(msg, "wrote map.md") {
			t.Fatalf("write after accept: %q, %v", msg, err)
		}
	})

	t.Run("the write entry keeps the prior bytes for rewind", func(t *testing.T) {
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
	h, _ := newTestHostDB(t)
	sid := seedSession(t, h)
	user, _ := h.brainParts()
	abs := filepath.Join(user.Dir, "style.md")
	msg, err := h.memoryTool(h.PipelineFor(), sid, tools.MemoryCall{Path: abs, Content: "---\ndescription: d\nread_if: r\n---\nbody"})
	if err == nil || !strings.Contains(err.Error(), "user-brain files are written with the file tools") {
		t.Fatalf("user brain: %q, %v", msg, err)
	}
}

// TestMemoryCommandsRegistered pins the /memory command step.
func TestMemoryCommandsRegistered(t *testing.T) {
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
