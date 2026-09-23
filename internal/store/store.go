// Package store is SQLite persistence (DESIGN §7): a global DB and one per project.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// DB is one SQLite database handle. All methods are safe for concurrent
// use (database/sql serializes).
type DB struct {
	db   *sql.DB
	path string // named in errors: there is a global and a per-project one
}

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
	kind TEXT NOT NULL,
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

// migrations[i] upgrades a database at user_version i to i+1. The schema
// const only ever creates what is missing, so a column added to an existing
// table needs its ALTER here as well as its place in schema: fresh databases
// get it from schema, existing ones from the migration. Append only, never
// edit or reorder a shipped entry.
var migrations = [][]string{
	{`ALTER TABLE tool_calls DROP COLUMN input_json`,
		`ALTER TABLE tool_calls DROP COLUMN error_kind`,
		`ALTER TABLE tool_calls DROP COLUMN latency_ms`,
		`ALTER TABLE tool_calls DROP COLUMN denied`,
		`ALTER TABLE tool_calls DROP COLUMN decided_by`,
		`ALTER TABLE sessions DROP COLUMN summary_message_id`},
	{`DROP TRIGGER IF EXISTS memory_ai`,
		`DROP TRIGGER IF EXISTS memory_ad`,
		`DROP TRIGGER IF EXISTS memory_au`,
		`DROP TABLE IF EXISTS memory_fts`,
		`DROP INDEX IF EXISTS memory_scope_hit`,
		`ALTER TABLE memory DROP COLUMN kind`,
		`ALTER TABLE memory DROP COLUMN tags`,
		`ALTER TABLE memory DROP COLUMN hit_count`,
		`ALTER TABLE memory DROP COLUMN last_hit_at`,
		`ALTER TABLE memory ADD COLUMN quote TEXT NOT NULL DEFAULT ''`},
}

func (s *DB) migrate() error {
	// one transaction, so a second process opening the same file waits and
	// then reads the version this one stamped
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	defer tx.Rollback()
	// A database this call creates gets every column from schema, so it is
	// current by construction and must be stamped: otherwise the first
	// migration ever added would run its ALTER against a table that already
	// has the column, and no new project could open its store.
	var name string
	fresh := errors.Is(tx.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' LIMIT 1`).Scan(&name), sql.ErrNoRows)
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	version := len(migrations)
	if !fresh {
		if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			return fmt.Errorf("store: read user_version: %w", err)
		}
	}
	if version > len(migrations) {
		return fmt.Errorf("store: %s is at schema version %d, this zemi knows %d; upgrade zemi", s.path, version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		for _, stmt := range migrations[i] {
			if _, err := tx.Exec(stmt); err != nil {
				return fmt.Errorf("store: migration %d: %w", i+1, err)
			}
		}
	}
	// PRAGMA takes no placeholder
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, len(migrations))); err != nil {
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

// sweep drops aged rows and reclaims their pages when it removed any.
func (s *DB) sweep() error {
	cps, err := s.db.Exec(`DELETE FROM checkpoints WHERE created_at < ?`,
		time.Now().Add(-CheckpointTTL).UnixMilli())
	if err != nil {
		return fmt.Errorf("store: sweep checkpoints: %w", err)
	}
	var m int64
	if sessionTTL > 0 {
		sess, err := s.db.Exec(`DELETE FROM sessions WHERE updated_at < ?`,
			time.Now().Add(-sessionTTL).UnixMilli())
		if err != nil {
			return fmt.Errorf("store: sweep sessions: %w", err)
		}
		m, _ = sess.RowsAffected()
	}
	n, _ := cps.RowsAffected()
	if n+m == 0 {
		return nil
	}
	// VACUUM rewrites the whole file; only the rare open that deleted pays it
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

// placeholders returns n comma-separated "?", or an empty string literal
// for none so IN () stays valid.
func placeholders(n int) string {
	if n == 0 {
		return "''"
	}
	return strings.Repeat(",?", n)[1:]
}

// ProjectDBPath is the per-project database location: a hash of the
// project root keeps the filename filesystem-safe. The caller passes the
// trust root, so every directory of a repository shares one database.
func ProjectDBPath(stateHome, projectRoot string) string {
	if projectRoot == "" {
		return filepath.Join(stateHome, "zemi.db")
	}
	sum := sha256.Sum256([]byte(projectRoot))
	return filepath.Join(stateHome, "projects", hex.EncodeToString(sum[:16]), "zemi.db")
}

// OpenForDirs opens the global database and, when a project root is known,
// the per-project one (DESIGN §7).
func OpenForDirs(stateHome, projectRoot string) (global *DB, project *DB, err error) {
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
	}
	return global, project, nil
}
