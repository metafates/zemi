package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestShortID: a pad file name is trusted as an id only through IDShape, so
// the generator and the regexp must agree.
func TestShortID(t *testing.T) {
	seen := map[string]bool{}
	for range 500 {
		id := ShortID()
		if !IDShape.MatchString(id) || seen[id] {
			t.Fatalf("ShortID() = %q, shape %v, repeat %v", id, IDShape.MatchString(id), seen[id])
		}
		seen[id] = true
	}
	for _, bad := range []string{"0123456", "0123456i", "0123456L", "../01234", "012345678"} {
		if IDShape.MatchString(bad) {
			t.Errorf("IDShape matches %q", bad)
		}
	}
}

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "zemi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestMigrationKeepsData: a store an older zemi stamped with user_version and
// no goose table opens, keeps its rows and gets the baseline migration, which
// a second open does not run again.
func TestMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zemi.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sid := ShortID()
	if err := db.CreateSession(sid, "", "a session", ""); err != nil {
		t.Fatal(err)
	}
	db.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// a store from an older zemi: no goose table, and none of the columns a
	// later migration added, under the names it used
	for _, q := range []string{
		`DROP TABLE goose_db_version`,
		`ALTER TABLE messages DROP COLUMN row_meta`,
		`ALTER TABLE sessions RENAME COLUMN agent TO profile`,
		`PRAGMA user_version = 8`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	db, err = Open(path)
	if err != nil {
		t.Fatalf("a pre-goose store must open: %v", err)
	}
	defer db.Close()
	if n, err := db.SessionCount(); err != nil || n != 1 {
		t.Fatalf("sessions = %d, %v", n, err)
	}
	var version int
	if err := db.db.QueryRow(`SELECT max(version_id) FROM goose_db_version`).Scan(&version); err != nil || version != 4 {
		t.Fatalf("goose version = %d, %v; want 4", version, err)
	}
}

// TestConcurrentOpen: two processes creating one store at once, as restored
// terminal panes do, both start.
func TestConcurrentOpen(t *testing.T) {
	for range 30 {
		path := filepath.Join(t.TempDir(), "zemi.db")
		// two handles are two connections, which is what two processes hold
		errs := make(chan error, 2)
		for range 2 {
			go func() {
				db, err := Open(path)
				if err == nil {
					db.Close()
				}
				errs <- err
			}()
		}
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestSweepVacuum: the open that ages out checkpoints rewrites the file
// only when they were a quarter of it.
func TestSweepVacuum(t *testing.T) {
	for _, c := range []struct {
		name         string
		aged, kept   int
		wantVacuumed bool
	}{
		{"a few pages", 1, 20, false},
		{"most of the file", 20, 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "zemi.db")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.CreateSession("s", "", "t", ""); err != nil {
				t.Fatal(err)
			}
			blob := make([]byte, 16<<10)
			for i := range c.aged + c.kept {
				m := &Message{SessionID: "s", Role: "user", PartsJSON: `[]`}
				if err := db.AddMessage(m); err != nil {
					t.Fatal(err)
				}
				if err := db.AddCheckpoint("s", m.ID, Checkpoint{Path: "/f", Existed: true, Content: blob}); err != nil {
					t.Fatal(err)
				}
				if i < c.aged {
					if _, err := db.db.Exec(`UPDATE checkpoints SET created_at = 0 WHERE message_id = ?`, m.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			db.Close()
			if db, err = Open(path); err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var free int
			if err := db.db.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil {
				t.Fatal(err)
			}
			if (free == 0) != c.wantVacuumed {
				t.Fatalf("%d free pages, want vacuumed %v", free, c.wantVacuumed)
			}
		})
	}
}

// TestRetentionSweep: opening drops checkpoints past CheckpointTTL, keeps
// the ones inside it, and never deletes a session.
func TestRetentionSweep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zemi.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old", "new"} {
		if err := db.CreateSession(id, "", id, ""); err != nil {
			t.Fatal(err)
		}
		m := &Message{SessionID: id, Role: "user", PartsJSON: `[]`}
		if err := db.AddMessage(m); err != nil {
			t.Fatal(err)
		}
		if err := db.AddCheckpoint(id, m.ID, Checkpoint{Path: "/tmp/f", Existed: true, Content: []byte("x")}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.Exec(`UPDATE sessions SET updated_at = ? WHERE id = 'old'`,
		time.Now().AddDate(-2, 0, 0).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	// the live session's own checkpoint is old enough to go on its own
	if _, err := db.db.Exec(`UPDATE checkpoints SET created_at = ? WHERE session_id = 'new'`,
		time.Now().Add(-CheckpointTTL-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// a conversation is never deleted on zemi's own initiative
	if _, err := db.GetSession("old"); err != nil {
		t.Fatalf("a two year old session was deleted: %v", err)
	}
	if msgs, _ := db.Messages("old"); len(msgs) != 1 {
		t.Fatalf("messages of a kept session = %+v", msgs)
	}
	if counts, err := db.CheckpointPaths("new"); err != nil || len(counts) != 0 {
		t.Fatalf("aged checkpoints survived: %+v %v", counts, err)
	}
	if counts, err := db.CheckpointPaths("old"); err != nil || len(counts) != 1 {
		t.Fatalf("a checkpoint inside the window was dropped: %+v %v", counts, err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	db := openTest(t)
	if err := db.CreateSession("s1", "parent0", "first", "coder"); err != nil {
		t.Fatal(err)
	}
	s, err := db.GetSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "first" || s.Agent != "coder" || s.ParentID != "parent0" {
		t.Fatalf("unexpected session: %+v", s)
	}
	if _, err := db.GetSession("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := db.RenameSession("s1", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddUsage("s1", 10, 20, 0, 0, 0.5); err != nil {
		t.Fatal(err)
	}
	s, _ = db.GetSession("s1")
	if s.Title != "renamed" || s.PromptTokens != 10 || s.CompletionTokens != 20 || s.Cost != 0.5 {
		t.Fatalf("usage/rename not applied: %+v", s)
	}
	// a vanished session must report the miss, not swallow the update
	for _, err := range []error{
		db.RenameSession("ghost", "x"),
		db.AddUsage("ghost", 1, 1, 0, 0, 1),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	}
	// only a message makes a session recent: a late bill for a closed one must not
	db.db.Exec(`UPDATE sessions SET updated_at = 1 WHERE id = 's1'`)
	_ = db.AddUsage("s1", 1, 1, 0, 0, 0)
	if s, _ = db.GetSession("s1"); s.UpdatedAt != 1 {
		t.Fatalf("usage bumped updated_at to %d", s.UpdatedAt)
	}
	_ = db.AddMessage(&Message{SessionID: "s1", Role: "user", PartsJSON: "[]"})
	if s, _ = db.GetSession("s1"); s.UpdatedAt == 1 {
		t.Fatal("a message did not bump updated_at")
	}
	db.db.Exec(`UPDATE sessions SET updated_at = 1 WHERE id = 's1'`)
	if err := db.RenameSession("s1", "relabelled"); err != nil {
		t.Fatal(err)
	}
	if s, _ = db.GetSession("s1"); s.UpdatedAt != 1 || s.Title != "relabelled" {
		t.Fatalf("rename: %+v", s)
	}
	if err := db.CreateSession("top", "", "top", ""); err != nil {
		t.Fatal(err)
	}
	all, err := db.ListSessions(10)
	if err != nil || len(all) != 1 || all[0].ID != "top" {
		t.Fatalf("list must hold only the top-level session: %v %v", all, err)
	}
}

func TestMessagesAndToolCalls(t *testing.T) {
	db := openTest(t)
	if err := db.CreateSession("s1", "", "t", ""); err != nil {
		t.Fatal(err)
	}
	m := &Message{SessionID: "s1", Role: "user", PartsJSON: `[{"type":"text","text":"hi"}]`}
	if err := db.AddMessage(m); err != nil {
		t.Fatal(err)
	}
	if m.ID == "" || m.CreatedAt == 0 {
		t.Fatal("AddMessage must fill id and timestamp")
	}
	if err := db.AddMessage(&Message{ID: "m2", SessionID: "s1", Role: "assistant", PartsJSON: `[]`}); err != nil {
		t.Fatal(err)
	}
	msgs, err := db.Messages("s1")
	if err != nil || len(msgs) != 2 {
		t.Fatalf("messages: %v %v", msgs, err)
	}
	if msgs[0].ID != m.ID {
		t.Fatalf("messages not in order: %+v", msgs)
	}
	if n, err := db.MessageCount("s1"); err != nil || n != 2 {
		t.Fatalf("count = %d %v", n, err)
	}
	// Tool telemetry rows stay out of the picker preview; the call and the
	// message it points at land together.
	if err := db.AddToolCall(&Message{SessionID: "s1", Role: "tool", PartsJSON: `{}`},
		&ToolCall{Tool: "shell", OK: false}); err != nil {
		t.Fatal(err)
	}
	if stats, err := db.ToolCallStats("s1", ""); err != nil || len(stats) != 1 || stats[0].Calls != 1 {
		t.Fatalf("tool stats = %+v, %v", stats, err)
	}
	if stats, err := db.ToolCallStats("s1", msgs[1].ID); err != nil || len(stats) != 1 || stats[0].Failed != 1 {
		t.Fatalf("tool stats after a message = %+v, %v", stats, err)
	}
	if stats, err := db.ToolCallStats("s1", "nope"); err != nil || len(stats) != 0 {
		t.Fatalf("tool stats after an unknown message = %+v, %v", stats, err)
	}
	if err := db.AddToolCall(&Message{SessionID: "ghost", Role: "tool", PartsJSON: `{}`},
		&ToolCall{Tool: "shell", OK: true}); err == nil {
		t.Fatal("an unknown session must fail")
	}
	prev, err := db.SessionPreviews([]string{"s1"})
	if err != nil || prev["s1"].Count != 2 || prev["s1"].FirstJSON != m.PartsJSON {
		t.Fatalf("preview: %+v %v", prev, err)
	}
}

// TestUserMessagesTopLevel: composer recall draws only the user's own rows,
// so a subagent session's user rows (agent-written prompts) stay out.
func TestUserMessagesTopLevel(t *testing.T) {
	db := openTest(t)
	if err := db.CreateSession("top", "", "t", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession("child", "top", "", ""); err != nil {
		t.Fatal(err)
	}
	top := &Message{SessionID: "top", Role: "user", PartsJSON: `{"text":"mine"}`}
	child := &Message{SessionID: "child", Role: "user", PartsJSON: `{"text":"prompt"}`}
	if err := db.AddMessage(top); err != nil {
		t.Fatal(err)
	}
	if err := db.AddMessage(child); err != nil {
		t.Fatal(err)
	}
	got, err := db.UserMessages(10)
	if err != nil || len(got) != 1 {
		t.Fatalf("UserMessages = %+v, %v; want only the top-level row", got, err)
	}
	if got[0].ID != top.ID || got[0].SessionID != "top" {
		t.Fatalf("recall picked the child row: %+v", got)
	}
}

// TestAddCompaction: summary and tail copies land together, and a write that fails leaves none of them behind.
func TestAddCompaction(t *testing.T) {
	db := openTest(t)
	if err := db.CreateSession("s1", "", "t", ""); err != nil {
		t.Fatal(err)
	}
	summary := &Message{Role: "user", PartsJSON: `{"summary":1}`}
	tail := []*Message{{Role: "tail", PartsJSON: `{"a":1}`}, {Role: "tail", PartsJSON: `{"b":2}`}}
	if err := db.AddCompaction("s1", summary, tail); err != nil {
		t.Fatal(err)
	}
	msgs, err := db.Messages("s1")
	if err != nil || len(msgs) != 3 || msgs[0].ID != summary.ID {
		t.Fatalf("messages = %+v, %v", msgs, err)
	}
	if err := db.AddCompaction("ghost", &Message{Role: "user", PartsJSON: `{}`},
		[]*Message{{Role: "tail", PartsJSON: `{}`}}); err == nil {
		t.Fatal("an unknown session must fail")
	}
	if msgs, _ := db.Messages("ghost"); len(msgs) != 0 {
		t.Fatalf("rolled back rows remain: %+v", msgs)
	}
}

func TestForkSession(t *testing.T) {
	db := openTest(t)
	if err := db.CreateSession("s1", "", "t", "p"); err != nil {
		t.Fatal(err)
	}
	summary := &Message{Role: "user", PartsJSON: `{"summary":1}`}
	if err := db.AddCompaction("s1", summary, []*Message{{Role: "tail", PartsJSON: `{}`}}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddCheckpoint("s1", summary.ID, Checkpoint{Path: "/f", Existed: true, Link: true, Content: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	src, _ := db.GetSession("s1")

	// a write failing partway leaves no fork behind
	if _, err := db.db.Exec(`CREATE TEMP TRIGGER fail BEFORE INSERT ON checkpoints BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.ForkSession(src, "bad", "t (fork)"); err == nil {
		t.Fatal("want the checkpoint error")
	}
	if _, err := db.GetSession("bad"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("half-copied fork: %v", err)
	}
	if msgs, _ := db.Messages("bad"); len(msgs) != 0 {
		t.Fatalf("half-copied messages: %+v", msgs)
	}
	if _, err := db.db.Exec(`DROP TRIGGER temp.fail`); err != nil {
		t.Fatal(err)
	}

	if err := db.ForkSession(src, "s2", "t (fork)"); err != nil {
		t.Fatal(err)
	}
	fork, err := db.GetSession("s2")
	if err != nil || fork.Title != "t (fork)" || fork.Agent != "p" || fork.ParentID != "" {
		t.Fatalf("fork = %+v, %v", fork, err)
	}
	msgs, _ := db.Messages("s2")
	if len(msgs) != 2 || msgs[0].ID == summary.ID || msgs[0].PartsJSON != summary.PartsJSON {
		t.Fatalf("messages = %+v", msgs)
	}
	if cps, err := db.CheckpointsFrom("s2", msgs[0].ID); err != nil || len(cps) != 1 || !cps[0].Link {
		t.Fatalf("checkpoints = %+v, %v", cps, err)
	}
}

// TestDeleteSession: a deleted session takes its subagent runs and every
// row under them, and its text is gone from the files on disk.
func TestDeleteSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zemi.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const secret = "sk-deleteme-0123456789"
	for _, s := range [][2]string{{"s1", ""}, {"c1", "s1"}, {"keep", ""}} {
		if err := db.CreateSession(s[0], s[1], "t", ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, sid := range []string{"s1", "c1"} {
		m := &Message{SessionID: sid, Role: "user", PartsJSON: `{"text":"` + secret + `"}`}
		if err := db.AddToolCall(m, &ToolCall{Tool: "shell"}); err != nil {
			t.Fatal(err)
		}
		if err := db.AddCheckpoint(sid, m.ID, Checkpoint{Path: "/f", Existed: true, Content: []byte(secret)}); err != nil {
			t.Fatal(err)
		}
		if err := db.SetTodos(sid, []Todo{{Content: secret, Status: "pending"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.DeleteSession("s1"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.db.QueryRow(`SELECT (SELECT COUNT(*) FROM sessions WHERE id != 'keep') + (SELECT COUNT(*) FROM messages) +
		(SELECT COUNT(*) FROM tool_calls) + (SELECT COUNT(*) FROM checkpoints) + (SELECT COUNT(*) FROM todos)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d rows left, %v", n, err)
	}
	for _, f := range []string{path, path + "-wal"} {
		if b, _ := os.ReadFile(f); strings.Contains(string(b), secret) {
			t.Fatalf("%s still holds the text", filepath.Base(f))
		}
	}
	if err := db.DeleteSession("s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestTodosReplace(t *testing.T) {
	db := openTest(t)
	if err := db.CreateSession("s1", "", "t", ""); err != nil {
		t.Fatal(err)
	}
	in := []Todo{{Content: "a", Status: "pending"}, {Content: "b", Status: "done", ActiveForm: "doing b"}}
	if err := db.SetTodos("s1", in); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTodos("s1", in); err != nil {
		t.Fatal(err)
	}
	out, err := db.Todos("s1")
	if err != nil || len(out) != 2 {
		t.Fatalf("todos: %v %v", out, err)
	}
	if out[1].Content != "b" {
		t.Fatalf("order wrong: %+v", out)
	}
}

func TestKVRoundtrip(t *testing.T) {
	db := openTest(t)
	if _, err := db.GetKV("k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := db.SetKV("k", `{"v":1}`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetKV("k", `{"v":2}`); err != nil {
		t.Fatal(err)
	}
	v, err := db.GetKV("k")
	if err != nil || v != `{"v":2}` {
		t.Fatalf("kv: %q %v", v, err)
	}
}

func TestKVOnceClaim(t *testing.T) {
	db := openTest(t)
	// only the first of two claims on the same key wins (a cross-process
	// reflection claim relies on this being atomic)
	if ok, err := db.SetKVOnce("claim:x", "1"); err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if ok, err := db.SetKVOnce("claim:x", "2"); err != nil || ok {
		t.Fatalf("second claim must not win: ok=%v err=%v", ok, err)
	}
	// a claim its owner crashed leaving is reclaimable once stale, not while fresh
	nowMs := time.Now().UnixMilli()
	if _, err := db.db.Exec(`UPDATE kv SET updated_at = ? WHERE key = 'claim:x'`, nowMs-60000); err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.ReclaimKVIfStale("claim:x", "3", nowMs-30000); !ok {
		t.Fatal("a stale claim must be reclaimed")
	}
	if ok, _ := db.ReclaimKVIfStale("claim:x", "4", nowMs-30000); ok {
		t.Fatal("a fresh claim must not be reclaimed")
	}
}

func TestProjectDBPath(t *testing.T) {
	if got := ProjectDBPath("/state", ""); got != "/state/zemi.db" {
		t.Fatalf("global path: %s", got)
	}
	p := ProjectDBPath("/state", "/home/u/proj")
	if filepath.Dir(filepath.Dir(p)) != filepath.Join("/state", "projects") {
		t.Fatalf("project path: %s", p)
	}
	if p == ProjectDBPath("/state", "/home/u/other") {
		t.Fatal("different projects must hash differently")
	}
}

// TestProjectRoot: opening a project store names its root beside it, a
// root file names a root only when that hashes to its dir, and Touch moves
// its last use.
func TestProjectRoot(t *testing.T) {
	const root = "/tmp/someproj"
	for _, c := range []struct {
		name   string
		before string // the root file before the open; "" for none
		valid  bool   // it names root then
	}{
		{"a first open writes it", "", false},
		{"a reopen touches it", root + "\n", true},
		{"another root's file is rewritten", "/tmp/other\n", false},
		{"a cut file is rewritten", root, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := t.TempDir()
			dir := filepath.Dir(ProjectDBPath(state, root))
			hour := time.Now().Add(-time.Hour)
			if c.before != "" {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "root"), []byte(c.before), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(filepath.Join(dir, "root"), hour, hour); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, ok := ProjectRoot(dir); ok != c.valid {
				t.Fatalf("before the open ok = %v, want %v", ok, c.valid)
			}
			g, p, err := OpenForDirs(state, root)
			if err != nil {
				t.Fatal(err)
			}
			g.Close()
			defer p.Close()
			got, opened, ok := ProjectRoot(dir)
			if !ok || got != root || !opened.After(hour.Add(time.Minute)) {
				t.Fatalf("after the open: %q %v %v", got, opened, ok)
			}
			if err := os.Chtimes(filepath.Join(dir, "root"), hour, hour); err != nil {
				t.Fatal(err)
			}
			if !p.Touch() {
				t.Fatal("Touch found no root file")
			}
			if _, opened, _ = ProjectRoot(dir); !opened.After(hour.Add(time.Minute)) {
				t.Fatalf("after Touch the last use is %v", opened)
			}
		})
	}
}

// TestClaimForDelete: a store nobody else holds is claimed with every
// session id and out of WAL, one another handle holds open is ErrInUse and
// that handle keeps working, and a missing or unreadable one claims nothing.
func TestClaimForDelete(t *testing.T) {
	for _, c := range []struct {
		name    string
		missing bool
		garbage bool
		held    bool
		want    []string
		err     error
	}{
		{name: "a free store", want: []string{"a", "b", "child"}},
		{name: "a store open elsewhere", held: true, err: ErrInUse},
		{name: "a missing store", missing: true},
		{name: "a store that is not a database", garbage: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "zemi.db")
			var db *DB
			if c.garbage {
				if err := os.WriteFile(path, []byte(strings.Repeat("not a database ", 64)), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if !c.missing {
				var err error
				db, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
				for _, s := range []struct{ id, parent string }{{"a", ""}, {"b", ""}, {"child", "a"}} {
					if err := db.CreateSession(s.id, s.parent, s.id, ""); err != nil {
						t.Fatal(err)
					}
				}
				if n, err := db.SessionCount(); err != nil || n != 2 {
					t.Fatalf("SessionCount = %d, %v; want the 2 top-level sessions", n, err)
				}
				if c.held {
					defer db.Close()
				} else {
					db.Close()
				}
			}
			ids, release, err := ClaimForDelete(path)
			if !errors.Is(err, c.err) {
				t.Fatalf("err = %v, want %v", err, c.err)
			}
			if c.held {
				if err := db.CreateSession("c", "", "c", ""); err != nil {
					t.Fatalf("the holder broke: %v", err)
				}
			}
			if err != nil {
				return
			}
			defer release()
			slices.Sort(ids)
			if !slices.Equal(ids, c.want) {
				t.Fatalf("ids = %v, want %v", ids, c.want)
			}
			if _, err := os.Stat(path + "-wal"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the claim left a -wal file: %v", err)
			}
		})
	}
}

func TestUsageSince(t *testing.T) {
	db := openTest(t)
	for _, s := range []struct{ id, parent string }{{"a", ""}, {"b", ""}, {"child", "a"}} {
		if err := db.CreateSession(s.id, s.parent, s.id, ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AddUsage(s.id, 100, 10, 0, 0, 0.5); err != nil {
			t.Fatal(err)
		}
	}
	u, err := db.UsageSince(0)
	if err != nil || u.Sessions != 2 || u.Prompt != 200 || u.Completion != 20 || u.Cost != 1 {
		t.Fatalf("usage = %+v, %v", u, err)
	}
	if u, _ := db.UsageSince(now() + 60_000); u.Sessions != 0 {
		t.Fatalf("future window = %+v", u)
	}
}
