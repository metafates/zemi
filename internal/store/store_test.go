package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/metafates/zemi/internal/perm"
)

// TestNewestMigration: the migrations' version is the highest number in their
// file names, so a number nobody used still leaves a store read as migrated.
func TestNewestMigration(t *testing.T) {
	files := fstest.MapFS{
		"migrations/0001_initial.sql": {},
		"migrations/0003_gap.sql":     {},
		"migrations/notes.sql":        {},
	}
	if got := newestMigration(files); got != 3 {
		t.Fatalf("newest migration = %d, want 3", got)
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
	if !errors.Is(err, ErrUnmigrated) || !strings.Contains(err.Error(), perm.TildePath(path)) || !strings.Contains(err.Error(), "start zemi") {
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
