package hostapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/agent"
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
// tool: the quote rule, the near-duplicate refusal, the per-scope budget,
// what replaces does with and without text, and /memory over budget.
func TestMemoryGate(t *testing.T) {
	filler := func(i int) string { return strings.TrimSpace(strings.Repeat(fmt.Sprintf("w%02d ", i), 65)) } // 259 bytes, no content token
	fill := func(t *testing.T, h *Host, n int) (oldest string) {
		t.Helper()
		for i := range n {
			m := &store.Memory{Scope: "project", Text: filler(i), ProvenanceSessionID: "seed", Origin: store.OriginLearned, UpdatedAt: int64(1 + i)}
			if err := h.DB.AddMemory(m); err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				oldest = m.ID
			}
		}
		return oldest
	}
	learned := func(t *testing.T, h *Host, scope store.Scope, text string) string {
		t.Helper()
		m := &store.Memory{Scope: scope, Text: text, ProvenanceSessionID: "seed", Origin: store.OriginLearned}
		if err := h.memoryDB(scope).AddMemory(m); err != nil {
			t.Fatal(err)
		}
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
				if rows, _ := h.GlobalDB.ListMemory("user", false); len(rows) != 1 || len(projectMemories(t, h)) != 0 {
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
			name: "full scope queues", text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string { fill(t, h, 11); return "" },
			want: "(project memory is full; replace one of ",
			check: func(t *testing.T, h *Host, ui *fakeUI, _ string) {
				if p := h.memoryProposals(); len(p) != 1 {
					t.Fatalf("proposals: %+v", p)
				}
			},
		},
		{
			name: "accepting into a full scope puts it back", autonomy: "ask", text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string { fill(t, h, 11); return "" },
			want: "queued for the user's review",
			check: func(t *testing.T, h *Host, ui *fakeUI, _ string) {
				p := h.memoryProposals()
				if _, err := h.acceptProposal(p[0].ID); err == nil || !strings.Contains(err.Error(), "project memory is full") {
					t.Fatalf("accept: %v", err)
				}
				if len(h.memoryProposals()) != 1 || len(projectMemories(t, h)) != 11 {
					t.Fatal("stored over budget")
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
			},
		},
		{
			name: "moving into a full scope refuses", text: "the ci runs on every push",
			seed: func(t *testing.T, h *Host, sid string) string {
				fill(t, h, 11)
				learned(t, h, "user", "the deploy script lives in bin/deploy")
				return ""
			},
			want: "(project memory is full",
			check: func(t *testing.T, h *Host, ui *fakeUI, _ string) {
				rows, _ := h.GlobalDB.ListMemory("user", false)
				if err := h.moveMemory(rows[0].ID, "user", "project"); err == nil || !strings.Contains(err.Error(), "project memory is full") {
					t.Fatalf("move: %v", err)
				}
				if rows, _ = h.GlobalDB.ListMemory("user", false); len(rows) != 1 || len(projectMemories(t, h)) != 11 {
					t.Fatal("moved over budget")
				}
			},
		},
		{
			name: "full scope with replaces stores", text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string { return fill(t, h, 11) },
			want: "saved as ",
			check: func(t *testing.T, h *Host, ui *fakeUI, replaces string) {
				if old, _ := h.DB.GetMemory(replaces); old.SupersededBy == "" {
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
				if _, err := h.DB.GetMemory(replaces); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("not deleted: %v", err)
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
			name: "/memory over budget points at the oldest", note: true, text: "the deploy script lives in bin/deploy",
			seed: func(t *testing.T, h *Host, sid string) string { return fill(t, h, 11) },
			want: "project memory is full; forget one of its oldest in /memory first",
			check: func(t *testing.T, h *Host, ui *fakeUI, replaces string) {
				if len(projectMemories(t, h)) != 11 {
					t.Fatal("stored over budget")
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
				msg, err = h.memoryTool(h.PipelineFor(), sid, cmp.Or(tc.scope, store.ScopeProject), tc.text, tc.quote, replaces)
			}
			if err != nil {
				msg = err.Error()
			}
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("answer %q (err %v), want %q", msg, err, tc.want)
			}
			if strings.HasSuffix(tc.want, "one of ") {
				if live, _ := h.DB.ListMemory("project", false); !strings.Contains(msg, live[len(live)-1].ID[:8]) {
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
			h.Settings.SetQuiet("memory.autonomy", "auto")
			h.SetProjectTrusted(!tc.untrusted)
			sid := seedSession(t, h)
			child := store.NewID()
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
// origins may never delete through Lua; user origin may, by id prefix.
func TestMemoryDelOriginGate(t *testing.T) {
	h, r := newTestHostDB(t)
	seedSession(t, h)
	m := &store.Memory{Scope: "project", Text: "old note about the pipeline", ProvenanceSessionID: "seed", Origin: store.OriginLearned}
	if err := h.DB.AddMemory(m); err != nil {
		t.Fatal(err)
	}
	err := r.ExecuteChunk(context.Background(), `zemi.memory.del("`+m.ID+`")`, "model.lua", lua.OriginProject)
	if err == nil || !strings.Contains(err.Error(), "only user config") {
		t.Fatalf("model origin allowed: %v", err)
	}
	runUser(t, r, `
		local ok, err = zemi.memory.del("nope")
		assert(ok == nil and err:find("no such memory"), tostring(err))
		assert(zemi.memory.del("`+m.ID[:8]+`"))
	`)
	if _, err := h.DB.GetMemory(m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("forget: %v", err)
	}
}

// TestDigestAtSessionStart covers §10.7: the session's prompt carries the
// memories it started with (TestPromptPinned keeps later ones out).
func TestDigestAtSessionStart(t *testing.T) {
	h, _ := newTestHostDB(t)

	if sys := h.buildSystem(nil); strings.Contains(sys, "# Memory") {
		t.Fatal("digest must be absent before any memory")
	}

	if err := h.Remember("the deploy script lives in bin/deploy"); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if err := h.ensureSession(nil, "hello"); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}

	sys := h.buildSystem(nil)
	if !strings.Contains(sys, "# Memory\nLines earlier sessions kept, as [id who age].") {
		t.Fatalf("digest missing from system prompt:\n%s", sys)
	}
	if !strings.Contains(sys, "## this project\n- [") || !strings.Contains(sys, " user] the deploy script lives in bin/deploy") {
		t.Fatalf("digest line missing:\n%s", sys)
	}

	if err := h.Remember("later note"); err != nil {
		t.Fatalf("remember2: %v", err)
	}
	if got := h.digestFor(); !strings.Contains(got, "later note") {
		t.Fatalf("digestFor missed the new memory: %q", got)
	}
}

// TestMemoryListActions drives the /memory list's actions by id: each
// applies at once, and as many undos as actions applied bring the stores
// back to where they started.
func TestMemoryListActions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cmds []string
		want string // the stores after cmds; "" when a command fails and nothing moves
	}{
		{"keep a suggestion", []string{"accept p1"}, "?project:deploy runs bin/deploy, ?project:the build runs with make check, ?user:forget:short replies, project:deploy by hand, project:run make check first, user:short replies, user:the build runs with make check"},
		{"keep a replacement retires what it replaces", []string{"accept p2"}, "?project:run make check first, ?project:the build runs with make check, ?user:forget:short replies, project:deploy runs bin/deploy, user:short replies, user:the build runs with make check"},
		{"keep a forget", []string{"accept p3"}, "?project:deploy runs bin/deploy, ?project:run make check first, ?project:the build runs with make check, project:deploy by hand, user:the build runs with make check"},
		{"drop a suggestion", []string{"reject p1"}, "?project:deploy runs bin/deploy, ?project:the build runs with make check, ?user:forget:short replies, project:deploy by hand, user:short replies, user:the build runs with make check"},
		{"forget a memory", []string{"forget m1 --yes"}, "?project:deploy runs bin/deploy, ?project:run make check first, ?project:the build runs with make check, ?user:forget:short replies, user:short replies, user:the build runs with make check"},
		{"move a memory everywhere", []string{"move m1"}, "?project:deploy runs bin/deploy, ?project:run make check first, ?project:the build runs with make check, ?user:forget:short replies, user:deploy by hand, user:short replies, user:the build runs with make check"},
		{"move a suggestion everywhere", []string{"move p1"}, "?project:deploy runs bin/deploy, ?project:the build runs with make check, ?user:forget:short replies, ?user:run make check first, project:deploy by hand, user:short replies, user:the build runs with make check"},
		{"a suggestion the other scope holds stays", []string{"move p4"}, ""},
		{"a forget suggestion does not move", []string{"move p3"}, ""},
		{"undo goes newest first", []string{"reject p1", "move m2", "forget m1 --yes"}, "?project:deploy runs bin/deploy, ?project:the build runs with make check, ?user:forget:short replies, project:short replies, user:the build runs with make check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHostDB(t)
			for _, m := range []*store.Memory{
				{ID: "m1", Scope: "project", Text: "deploy by hand", Origin: store.OriginUser},
				{ID: "m2", Scope: "user", Text: "short replies", Origin: store.OriginLearned},
				{ID: "m3", Scope: "user", Text: "the build runs with make check", Origin: store.OriginLearned},
			} {
				m.ProvenanceSessionID = "seed"
				if err := h.memoryDB(m.Scope).AddMemory(m); err != nil {
					t.Fatal(err)
				}
			}
			for _, p := range []memoryProposal{
				{ID: "p1", Scope: "project", Text: "run make check first"},
				{ID: "p2", Scope: "project", Text: "deploy runs bin/deploy", Supersedes: "m1"},
				{ID: "p3", Scope: "user", Text: "short replies", Supersedes: "m2", Forget: true},
				{ID: "p4", Scope: "project", Text: "the build runs with make check"},
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
					out = append(out, string(m.Scope)+":"+m.Text)
				}
				for _, p := range h.memoryProposals() {
					out = append(out, "?"+string(p.Scope)+":"+map[bool]string{true: "forget:"}[p.Forget]+p.Text)
				}
				slices.Sort(out)
				return strings.Join(out, ", ")
			}
			start, applied := stores(), 0
			for _, c := range tc.cmds {
				if err := h.memoryCommand(c); err == nil {
					applied++
				} else if tc.want != "" {
					t.Fatalf("%s: %v", c, err)
				}
			}
			if got := stores(); got != cmp.Or(tc.want, start) {
				t.Fatalf("after %v:\n got %s\nwant %s", tc.cmds, got, cmp.Or(tc.want, start))
			}
			for range applied {
				if err := h.memoryCommand("undo"); err != nil {
					t.Fatal(err)
				}
			}
			if got := stores(); got != start {
				t.Fatalf("undone:\n got %s\nwant %s", got, start)
			}
			if err := h.memoryCommand("undo"); err == nil || !strings.Contains(err.Error(), "nothing to undo") {
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
				{ID: "m1", Scope: "project", Text: "deploy by hand", ProvenanceSessionID: "seed", Origin: store.OriginLearned},
				{ID: "m2", Scope: "project", Text: "other note", ProvenanceSessionID: "seed", Origin: store.OriginLearned},
			} {
				if err := h.DB.AddMemory(m); err != nil {
					t.Fatal(err)
				}
			}
			p := memoryProposal{ID: "p1", Scope: "project", Text: "suggested note", ProvenanceSessionID: "seed", Origin: store.OriginLearned}
			if err := h.addProposal(p); err != nil {
				t.Fatal(err)
			}
			ui := &fakeUI{edit: func(path string) { _ = os.WriteFile(path, []byte(tc.text), 0o600) }}
			h.editMemory(ui, tc.row)

			m, err := h.DB.GetMemory(tc.row)
			switch {
			case tc.want == "" && tc.row == "m1":
				if m.Text != "deploy by hand" || m.Origin != store.OriginLearned {
					t.Fatalf("changed: %+v", m)
				}
			case err != nil || m.Text != tc.want || m.Origin != store.OriginUser:
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
	learned := &store.Memory{Scope: "user", Text: "Deploy by hand", ProvenanceSessionID: "seed", Origin: store.OriginLearned}
	if err := h.GlobalDB.AddMemory(learned); err != nil {
		t.Fatal(err)
	}
	if err := h.moveMemory(projectMemories(t, h)[0].ID, "project", "user"); err != nil {
		t.Fatal(err)
	}
	if live, _ := h.GlobalDB.ListMemory("user", false); len(live) != 1 || live[0].Origin != store.OriginUser || len(projectMemories(t, h)) != 0 {
		t.Fatalf("merge: %+v", live)
	}
}

func TestAcceptProposalKeepsItOnStoreError(t *testing.T) {
	h, _ := newTestHostDB(t)
	// no origin: the store refuses the row, after the proposal was taken
	payload, _ := json.Marshal(memoryProposal{ID: "p1", Scope: "project", Text: "unstorable"})
	if err := h.DB.AddProposal(&store.Proposal{ID: "p1", Payload: string(payload)}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.acceptProposal("p1"); err == nil {
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
	if err := h.Remember("deploy by hand"); err != nil {
		t.Fatal(err)
	}
	// typed, a forget asks first: headless it names the --yes form, and with
	// a screen /memory opens on that memory
	id := projectMemories(t, h)[0].ID
	if err := h.memoryCommand("forget " + id[:8]); err == nil || !strings.Contains(err.Error(), "/memory forget "+id+" --yes runs it without asking") {
		t.Fatalf("a headless forget: %v", err)
	}
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.memoryCommand("forget " + id[:8]); err != nil {
		t.Fatal(err)
	}
	confirm := "memory forget " + id + " --yes"
	if o := ui.overlays(); len(o) != 1 || o[0].Confirm != confirm || !strings.Contains(fmt.Sprint(o[0].Items), confirm) || len(projectMemories(t, h)) != 1 {
		t.Fatalf("a typed forget: %+v, %d memories", o, len(projectMemories(t, h)))
	}
	if err := h.memoryCommand("forget " + id[:8] + " --yes"); err != nil {
		t.Fatal(err)
	}
	if n := h.evlog.notes[len(h.evlog.notes)-1]; n.title != "Memory forgotten" || n.body != "deploy by hand" {
		t.Fatalf("toast: %+v", n)
	}
	// opening the list starts with nothing to undo
	if err := h.memoryCommand(""); err != nil {
		t.Fatal(err)
	}
	if err := h.memoryCommand("undo"); err == nil {
		t.Fatal("an undo reached past the list's opening")
	}
}
