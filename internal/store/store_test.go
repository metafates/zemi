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

	"github.com/metafates/zemi/internal/perm"
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

// TestWriteSessionGone: a write whose session row another window deleted is
// ErrSessionGone in its own wording; any other failure stays itself.
func TestWriteSessionGone(t *testing.T) {
	// gone is a store whose session row another window deleted behind its back.
	gone := func(t *testing.T) (*DB, string) {
		t.Helper()
		db := openTest(t)
		sid := ShortID()
		if err := db.CreateSession(sid, "", "s", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.Exec(`DELETE FROM sessions WHERE id = ?`, sid); err != nil {
			t.Fatal(err)
		}
		return db, sid
	}
	// goneAs asserts err is ErrSessionGone in the writer's own wording.
	goneAs := func(t *testing.T, err error, want string) {
		t.Helper()
		if !errors.Is(err, ErrSessionGone) || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want ErrSessionGone wrapped as %q", err, want)
		}
	}
	t.Run("session gone: message", func(t *testing.T) {
		db, sid := gone(t)
		goneAs(t, db.AddMessage(&Message{SessionID: sid, Role: "user", PartsJSON: `"hi"`}), "store: add message")
	})
	t.Run("session gone: tool call", func(t *testing.T) {
		db, sid := gone(t)
		goneAs(t, db.AddToolCall(&Message{SessionID: sid, Role: "tool", PartsJSON: `{}`}, &ToolCall{Tool: "read"}), "store: add message")
	})
	t.Run("session gone: todos", func(t *testing.T) {
		db, sid := gone(t)
		goneAs(t, db.SetTodos(sid, []Todo{{Content: "a"}}), "store: set todos")
	})
	t.Run("another failure stays raw", func(t *testing.T) {
		db := openTest(t)
		sid := ShortID()
		if err := db.CreateSession(sid, "", "s", ""); err != nil {
			t.Fatal(err)
		}
		m := &Message{ID: "dup", SessionID: sid, Role: "user", PartsJSON: `{}`}
		if err := db.AddMessage(m); err != nil {
			t.Fatal(err)
		}
		if err := db.AddMessage(m); err == nil || errors.Is(err, ErrSessionGone) {
			t.Fatalf("err = %v, want some other failure", err)
		}
	})
}

// TestMigrationFresh: a fresh store opens, the one migration stamps its
// version, and a second open runs nothing again.
func TestMigrationFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zemi.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sid := ShortID()
	if err := db.CreateSession(sid, "", "a session", ""); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.db.QueryRow(`SELECT max(version_id) FROM goose_db_version`).Scan(&version); err != nil || version != migrationCount() {
		t.Fatalf("goose version = %d, %v; want %d", version, err, migrationCount())
	}
	db.Close()

	// the second open runs nothing again
	db, err = Open(path)
	if err != nil {
		t.Fatalf("a migrated store must reopen: %v", err)
	}
	defer db.Close()
	var again int
	if err := db.db.QueryRow(`SELECT max(version_id) FROM goose_db_version`).Scan(&again); err != nil || again != version {
		t.Fatalf("goose version after a reopen = %d, %v; want %d", again, err, version)
	}
	var n int
	if err := db.db.QueryRow(`SELECT count(*) FROM sessions WHERE ` + _topLevelSessions).Scan(&n); err != nil || n != 1 {
		t.Fatalf("sessions = %d, %v", n, err)
	}
}

// TestOpenReadUnmigrated: a read-only open refuses a store this binary's
// migrations have not reached, naming the store and the remedy, and migrates
// nothing; a corrupt file is refused as unreadable, not unmigrated; a store
// at the current schema reads.
func TestOpenReadUnmigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zemi.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OpenRead(path)
	if !errors.Is(err, ErrUnmigrated) || !strings.Contains(err.Error(), perm.TildePath(path)) || !strings.Contains(err.Error(), "zemi health") {
		t.Fatalf("a 0-byte store: %v, want ErrUnmigrated naming the store and the remedy", err)
	}

	// a file SQLite cannot read at all: no open migrates it, so the
	// unmigrated remedy would never work
	corrupt := filepath.Join(t.TempDir(), "zemi.db")
	if err := os.WriteFile(corrupt, []byte("not a database at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = OpenRead(corrupt)
	if err == nil || errors.Is(err, ErrUnmigrated) || !strings.Contains(err.Error(), "not a readable zemi store") {
		t.Fatalf("a corrupt store: %v, want a not-readable error, not ErrUnmigrated", err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ShortID(), "", "a session", ""); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = OpenRead(path)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	err = db.db.QueryRow(`SELECT count(*) FROM sessions WHERE ` + _topLevelSessions).Scan(&n)
	db.Close()
	if err != nil || n != 1 {
		t.Fatalf("a migrated store reads: %d, %v", n, err)
	}
	// query_only: a write through a read-only open fails, never half-applies
	db, err = OpenRead(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Query(`INSERT INTO kv VALUES ('x', '{}', 0)`); err == nil || !strings.Contains(err.Error(), "readonly") {
		db.Close()
		t.Fatalf("a write through a read-only open: %v, want a readonly refusal", err)
	}
	db.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE goose_db_version SET version_id = 0`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := OpenRead(path); !errors.Is(err, ErrUnmigrated) {
		t.Fatalf("a store behind the migrations: %v, want ErrUnmigrated", err)
	}
	raw, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := raw.QueryRow(`SELECT max(version_id) FROM goose_db_version`).Scan(&version); err != nil || version != 0 {
		raw.Close()
		t.Fatalf("a refused read migrated to %d, %v; want it untouched at 0", version, err)
	}
	raw.Close()
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
	if err := db.execOne("rename", `UPDATE sessions SET title = ? WHERE id = ?`, "renamed", "s1"); err != nil {
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
		db.execOne("rename", `UPDATE sessions SET title = ? WHERE id = ?`, "x", "ghost"),
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
	if err := db.execOne("rename", `UPDATE sessions SET title = ? WHERE id = ?`, "relabelled", "s1"); err != nil {
		t.Fatal(err)
	}
	if s, _ = db.GetSession("s1"); s.UpdatedAt != 1 || s.Title != "relabelled" {
		t.Fatalf("rename: %+v", s)
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
		&ToolCall{Tool: "shell", OK: false}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddToolCall(&Message{SessionID: "ghost", Role: "tool", PartsJSON: `{}`},
		&ToolCall{Tool: "shell", OK: true}); err == nil {
		t.Fatal("an unknown session must fail")
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
		{name: "a free store", want: []string{"a", "b", "child", MainSessionID}},
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
				var n int
				if err := db.db.QueryRow(`SELECT count(*) FROM sessions WHERE ` + _topLevelSessions).Scan(&n); err != nil || n != 2 {
					t.Fatalf("top-level sessions = %d, %v; want 2", n, err)
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
	// a bill lands in its own UTC day; the windows read the same sum while
	// the day is today, and nothing past it
	if err := db.AddUsageDay(200, 20, 0.5); err != nil {
		t.Fatal(err)
	}
	if u, err := db.UsageSince(time.Time{}); err != nil || u.Prompt != 200 || u.Completion != 20 || u.Cost != 0.5 {
		t.Fatalf("all time = %+v, %v", u, err)
	}
	y, m, d := time.Now().UTC().Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	if u, _ := db.UsageSince(midnight); u.Prompt != 200 {
		t.Fatalf("today = %+v, want the whole day's bills", u)
	}
	if u, _ := db.UsageSince(midnight.AddDate(0, 0, 1)); u.Prompt != 0 {
		t.Fatalf("tomorrow = %+v, want zero", u)
	}
}
