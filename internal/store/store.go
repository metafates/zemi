// Package store is SQLite persistence (DESIGN §7): a global DB and one per project.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// DB is one SQLite database handle. All methods are safe for concurrent
// use (database/sql serializes).
type DB struct {
	db   *sql.DB
	path string // named in errors: there is a global and a per-project one
	// memGen moves after every memory write, so a cache over the rows knows
	// to reload without each writer marking it
	memGen atomic.Uint64
}

// MemoryGen changes whenever a memory row was written.
func (s *DB) MemoryGen() uint64 { return s.memGen.Load() }

// Open creates the parent directory if needed and opens the database with
// WAL journalling. Every transaction takes the write lock at BEGIN, so one
// that reads first waits on busy_timeout for another process instead of
// failing its upgrade with SQLITE_BUSY.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("store: create dir: %w", err)
	}
	d, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	d.SetMaxOpenConns(1)
	s := &DB{db: d, path: path}
	// switching to WAL takes an exclusive lock that busy_timeout does not wait
	// for, so a second process creating the same store retries; the mode sticks
	// to the file, and one that never gets it runs with a rollback journal
	for range 100 {
		if _, err = d.Exec(`PRAGMA journal_mode = WAL`); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.migrate(); err != nil {
		d.Close()
		return nil, err
	}
	// best effort: a failed sweep must never keep zemi from starting
	_ = s.sweep()
	return s, nil
}

// OpenRead opens an existing store read-only: no migration, no sweep and
// no checkpoint on close, so a look waits on no lock a running zemi holds.
// A missing file is fs.ErrNotExist.
func OpenRead(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	d, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	s := &DB{db: d, path: path}
	var version int
	if err := d.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		d.Close()
		return nil, fmt.Errorf("store: read user_version: %w", err)
	}
	if err := s.checkVersion(version); err != nil {
		d.Close()
		return nil, err
	}
	return s, nil
}

func (s *DB) checkVersion(version int) error {
	if version != schemaVersion {
		return fmt.Errorf("store: %s has schema %d, this zemi needs %d: delete the file (zemi keeps no migrations yet)", s.path, version, schemaVersion)
	}
	return nil
}

// Close releases the handle.
func (s *DB) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id TEXT PRIMARY KEY,
	parent_id TEXT,
	title TEXT NOT NULL DEFAULT '',
	profile TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cost REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS sessions_parent ON sessions(parent_id);
-- the picker and the splash order by recency on every render
CREATE INDEX IF NOT EXISTS sessions_updated ON sessions(updated_at);

CREATE TABLE IF NOT EXISTS messages (
	id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	role TEXT NOT NULL,
	parts_json TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_session ON messages(session_id);

CREATE TABLE IF NOT EXISTS todos (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	content TEXT NOT NULL,
	status TEXT NOT NULL,
	active_form TEXT NOT NULL DEFAULT '',
	ord INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS todos_session ON todos(session_id);

CREATE TABLE IF NOT EXISTS tool_calls (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	tool TEXT NOT NULL,
	ok INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS tool_calls_message ON tool_calls(message_id, created_at);

CREATE TABLE IF NOT EXISTS checkpoints (
	message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	session_id TEXT NOT NULL,
	path TEXT NOT NULL,
	existed INTEGER NOT NULL,
	link INTEGER NOT NULL DEFAULT 0,
	content BLOB,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (message_id, path)
);
CREATE INDEX IF NOT EXISTS checkpoints_session ON checkpoints(session_id);

CREATE TABLE IF NOT EXISTS skills (
	name TEXT PRIMARY KEY,
	path TEXT NOT NULL,
	source TEXT NOT NULL,
	enabled INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS kv (
	key TEXT PRIMARY KEY,
	value_json TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS proposals (
	id TEXT PRIMARY KEY,
	payload TEXT NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS memory (
	id TEXT PRIMARY KEY,
	scope TEXT NOT NULL,
	text TEXT NOT NULL,
	quote TEXT NOT NULL DEFAULT '',
	provenance_session_id TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	superseded_by TEXT,
	origin TEXT NOT NULL
);
-- listings and the digest order by recency within a scope
CREATE INDEX IF NOT EXISTS memory_scope_updated ON memory(scope, updated_at);
`

// schemaVersion stamps every store. zemi keeps no migrations before its
// first release: a schema change bumps this, and a store another version made
// is refused with its path, for the developer to delete.
const schemaVersion = 4

func (s *DB) migrate() error {
	// one transaction, so a second process opening the same file waits and
	// then reads the version this one stamped
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: open schema: %w", err)
	}
	defer tx.Rollback()
	var name string
	fresh := errors.Is(tx.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' LIMIT 1`).Scan(&name), sql.ErrNoRows)
	if !fresh {
		var version int
		if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			return fmt.Errorf("store: read user_version: %w", err)
		}
		if err := s.checkVersion(version); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("store: schema: %w", err)
	}
	// PRAGMA takes no placeholder
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("store: set user_version: %w", err)
	}
	return tx.Commit()
}

// Retention windows (DESIGN §7 keeps no archive): checkpoints hold a whole
// file each, so they go first, and a session takes its messages, tool calls
// and todos with it. Both windows are generous, and the sweep runs at open,
// before the live session exists, so it can never cut the conversation in
// progress.
const (
	CheckpointTTL = 30 * 24 * time.Hour
	// 0 keeps every session forever: losing a conversation costs more than
	// the disk it holds, so zemi never deletes one on its own. Set a
	// duration here to sweep sessions nobody has touched since.
	sessionTTL = 0
)

// sweep drops aged rows and reclaims their pages once enough are free.
func (s *DB) sweep() error {
	if _, err := s.db.Exec(`DELETE FROM checkpoints WHERE created_at < ?`,
		time.Now().Add(-CheckpointTTL).UnixMilli()); err != nil {
		return fmt.Errorf("store: sweep checkpoints: %w", err)
	}
	if sessionTTL > 0 {
		if _, err := s.db.Exec(`DELETE FROM sessions WHERE updated_at < ?`,
			time.Now().Add(-sessionTTL).UnixMilli()); err != nil {
			return fmt.Errorf("store: sweep sessions: %w", err)
		}
	}
	// NOTE: VACUUM rewrites the whole file before the UI starts, so it waits
	// until a quarter of the pages are free; SQLite reuses the rest, and
	// DeleteSession vacuums on its own so a deleted conversation is gone.
	var free, pages int64
	if err := s.db.QueryRow(`SELECT * FROM pragma_freelist_count(), pragma_page_count()`).Scan(&free, &pages); err != nil || free*4 <= pages {
		return err
	}
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("store: vacuum: %w", err)
	}
	return nil
}

// NewID returns a random 128-bit id, hex encoded.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("store: entropy unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func now() int64 { return time.Now().UnixMilli() }

// nullString adapts Go "" to SQL NULL.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// jsonList is strings as one parameter for IN (SELECT value FROM json_each(?));
// never "null", which json_each reads as one NULL row that NOT IN never passes.
func jsonList(s []string) string {
	if len(s) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(s)
	return string(b)
}

type scanner interface{ Scan(...any) error }

// collect scans every row of a query and closes it.
func collect[T any](rows *sql.Rows, err error, scan func(scanner) (T, error)) ([]T, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// execOne runs an update or delete of one row by key; no match is ErrNotFound.
func (s *DB) execOne(what, query string, args ...any) error {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// ProjectDBPath is the per-project database location: a hash of the
// project root keeps the filename filesystem-safe. The caller passes the
// trust root, so every directory of a repository shares one database.
func ProjectDBPath(stateHome, projectRoot string) string {
	if projectRoot == "" {
		return filepath.Join(stateHome, "zemi.db")
	}
	return filepath.Join(stateHome, "projects", projectKey(projectRoot), "zemi.db")
}

func projectKey(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:16])
}

// OpenForDirs opens the global database and, when a project root is known,
// the per-project one (DESIGN §7), whose dir's root file it writes or, when
// it names the root already, touches: its mtime is the last open.
func OpenForDirs(stateHome, projectRoot string) (global, project *DB, err error) {
	global, err = Open(filepath.Join(stateHome, "zemi.db"))
	if err != nil {
		return nil, nil, err
	}
	if projectRoot != "" {
		project, err = Open(ProjectDBPath(stateHome, projectRoot))
		if err != nil {
			global.Close()
			return nil, nil, err
		}
		// best effort, like the sweep: a failed write never stops zemi
		dir := filepath.Dir(project.path)
		if _, _, ok := ProjectRoot(dir); ok {
			t := time.Now()
			_ = os.Chtimes(filepath.Join(dir, "root"), t, t)
		} else {
			_ = os.WriteFile(filepath.Join(dir, "root"), []byte(projectRoot+"\n"), 0o600)
		}
	}
	return global, project, nil
}

// ProjectRoot reads the root file of a project state dir: the trust root the
// dir belongs to and when zemi last opened it. It is ok only when the root
// hashes to the dir's name, so a copied or edited file names nothing.
func ProjectRoot(dir string) (root string, opened time.Time, ok bool) {
	f, err := os.Open(filepath.Join(dir, "root"))
	if err != nil {
		return "", time.Time{}, false
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 4096))
	root, ok = strings.CutSuffix(string(b), "\n")
	fi, err := f.Stat()
	if !ok || err != nil || projectKey(root) != filepath.Base(dir) {
		return "", time.Time{}, false
	}
	return root, fi.ModTime(), true
}
