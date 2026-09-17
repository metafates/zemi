package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "zemi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestSchemaVersion: an opened database carries the version the migration
// list ends at, and one written by a newer zemi is refused instead of read
// with columns this build does not know.
func TestSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zemi.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Fatalf("user_version = %d, want %d", version, len(migrations))
	}
	if _, err := db.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "upgrade zemi") {
		t.Fatalf("a newer database must be refused, got %v", err)
	}
}

// TestRetentionSweep: opening drops checkpoints past CheckpointTTL, keeps
// the ones inside it, and never deletes a session (sessionTTL is off).
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
		if err := db.AddCheckpoint(id, m.ID, "/tmp/f", true, []byte("x")); err != nil {
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
	if s.Title != "first" || s.Profile != "coder" || s.ParentID != "parent0" {
		t.Fatalf("unexpected session: %+v", s)
	}
	if _, err := db.GetSession("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := db.RenameSession("s1", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddUsage("s1", 10, 20, 0.5); err != nil {
		t.Fatal(err)
	}
	s, _ = db.GetSession("s1")
	if s.Title != "renamed" || s.PromptTokens != 10 || s.CompletionTokens != 20 || s.Cost != 0.5 {
		t.Fatalf("usage/rename not applied: %+v", s)
	}
	// a vanished session must report the miss, not swallow the update
	for _, err := range []error{
		db.RenameSession("ghost", "x"),
		db.AddUsage("ghost", 1, 1, 1),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
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
	// Tool telemetry rows stay out of the picker preview; the call and the
	// message it points at land together.
	if err := db.AddToolCall(&Message{SessionID: "s1", Role: "tool", PartsJSON: `{}`},
		&ToolCall{Tool: "bash", InputJSON: `{}`, OK: false, ErrorKind: "denied", Denied: true, DecidedBy: "user"}); err != nil {
		t.Fatal(err)
	}
	if stats, err := db.ToolCallStats("s1"); err != nil || len(stats) != 1 || stats[0].Calls != 1 {
		t.Fatalf("tool stats = %+v, %v", stats, err)
	}
	if err := db.AddToolCall(&Message{SessionID: "ghost", Role: "tool", PartsJSON: `{}`},
		&ToolCall{Tool: "bash", InputJSON: `{}`, OK: true}); err == nil {
		t.Fatal("an unknown session must fail")
	}
	prev, err := db.SessionPreviews([]string{"s1"})
	if err != nil || prev["s1"].Count != 2 || prev["s1"].FirstJSON != m.PartsJSON {
		t.Fatalf("preview: %+v %v", prev, err)
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
	if err := db.AddCheckpoint("s1", summary.ID, "/f", true, []byte("x")); err != nil {
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
	if err != nil || fork.Title != "t (fork)" || fork.Profile != "p" || fork.ParentID != "" {
		t.Fatalf("fork = %+v, %v", fork, err)
	}
	msgs, _ := db.Messages("s2")
	if len(msgs) != 2 || msgs[0].ID == summary.ID || msgs[0].PartsJSON != summary.PartsJSON {
		t.Fatalf("messages = %+v", msgs)
	}
	if counts, err := db.CheckpointPaths("s2"); err != nil || len(counts[msgs[0].ID]) != 1 {
		t.Fatalf("checkpoints = %+v, %v", counts, err)
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
		if err := db.AddToolCall(m, &ToolCall{Tool: "bash", InputJSON: `{"command":"echo ` + secret + `"}`}); err != nil {
			t.Fatal(err)
		}
		if err := db.AddCheckpoint(sid, m.ID, "/f", true, []byte(secret)); err != nil {
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
	if out[1].Content != "b" || out[1].Ord != 1 {
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

func TestOpenForDirs(t *testing.T) {
	state := t.TempDir()
	g, p, err := OpenForDirs(state, "/tmp/someproj")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	defer p.Close()
	if err := p.CreateSession("s1", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := g.SetKV("x", "1"); err != nil {
		t.Fatal(err)
	}
}

func TestUsageSince(t *testing.T) {
	db := openTest(t)
	for _, s := range []struct{ id, parent string }{{"a", ""}, {"b", ""}, {"child", "a"}} {
		if err := db.CreateSession(s.id, s.parent, s.id, ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AddUsage(s.id, 100, 10, 0.5); err != nil {
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
