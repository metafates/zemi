// Package store is SQLite persistence (DESIGN §7): a global DB and one per project.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	db *sql.DB
}

// Open creates the parent directory if needed and opens the database with
// WAL journalling.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: create dir: %w", err)
		}
	}
	d, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	d.SetMaxOpenConns(1)
	s := &DB{db: d}
	if err := s.migrate(); err != nil {
		d.Close()
		return nil, err
	}
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
	summary_message_id TEXT,
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cost REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS sessions_parent ON sessions(parent_id);

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
	input_json TEXT NOT NULL,
	ok INTEGER NOT NULL,
	error_kind TEXT NOT NULL DEFAULT '',
	latency_ms INTEGER NOT NULL DEFAULT 0,
	denied INTEGER NOT NULL DEFAULT 0,
	decided_by TEXT NOT NULL DEFAULT '',
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

CREATE TABLE IF NOT EXISTS config_revisions (
	id TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL,
	title TEXT NOT NULL,
	changes_json TEXT NOT NULL DEFAULT '',
	paths_json TEXT NOT NULL DEFAULT '[]',
	status TEXT NOT NULL DEFAULT 'pending',
	applied_by TEXT NOT NULL DEFAULT '',
	eval_ok INTEGER,
	commit_sha TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS memory (
	id TEXT PRIMARY KEY,
	scope TEXT NOT NULL,
	kind TEXT NOT NULL,
	text TEXT NOT NULL,
	tags TEXT NOT NULL DEFAULT '',
	provenance_session_id TEXT,
	confidence REAL NOT NULL DEFAULT 1,
	hit_count INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	last_hit_at INTEGER NOT NULL DEFAULT 0,
	superseded_by TEXT,
	origin TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS memory_scope_hit ON memory(scope, last_hit_at);

-- External-content FTS5 over memory.text; triggers keep it in sync
-- (DESIGN §7). The TEXT primary key leaves the implicit rowid intact.
CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(text, content='memory', content_rowid='rowid');
CREATE TRIGGER IF NOT EXISTS memory_ai AFTER INSERT ON memory BEGIN
	INSERT INTO memory_fts(rowid, text) VALUES (new.rowid, new.text);
END;
CREATE TRIGGER IF NOT EXISTS memory_ad AFTER DELETE ON memory BEGIN
	INSERT INTO memory_fts(memory_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
END;
CREATE TRIGGER IF NOT EXISTS memory_au AFTER UPDATE OF text ON memory BEGIN
	INSERT INTO memory_fts(memory_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
	INSERT INTO memory_fts(rowid, text) VALUES (new.rowid, new.text);
END;
`

// migrations[i] upgrades a database at user_version i to i+1. The schema
// const only ever creates what is missing, so a column added to an existing
// table needs its ALTER here as well as its place in schema: fresh databases
// get it from schema, existing ones from the migration. Append only, never
// edit or reorder a shipped entry.
var migrations = [][]string{}

func (s *DB) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("store: database is at schema version %d, this zemi knows %d; upgrade zemi", version, len(migrations))
	}
	if version == len(migrations) {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
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

// placeholders returns n comma-separated "?" ("”" for none, so IN () stays valid).
func placeholders(n int) string {
	if n == 0 {
		return "''"
	}
	return strings.Repeat(",?", n)[1:]
}

// ProjectDBPath is the per-project database location: a hash of the
// project root keeps the filename filesystem-safe.
func ProjectDBPath(stateHome, projectDir string) string {
	if projectDir == "" {
		return filepath.Join(stateHome, "zemi.db")
	}
	sum := sha256.Sum256([]byte(projectDir))
	return filepath.Join(stateHome, "projects", hex.EncodeToString(sum[:16]), "zemi.db")
}

// OpenForDirs opens the global database and, when a project root is known,
// the per-project one (DESIGN §7).
func OpenForDirs(stateHome, projectDir string) (global *DB, project *DB, err error) {
	global, err = Open(filepath.Join(stateHome, "zemi.db"))
	if err != nil {
		return nil, nil, err
	}
	if projectDir != "" {
		project, err = Open(ProjectDBPath(stateHome, projectDir))
		if err != nil {
			global.Close()
			return nil, nil, err
		}
	}
	return global, project, nil
}
