// Package store is SQLite persistence (DESIGN §7): a global DB and one per project.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base32"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/metafates/zemi/internal/filelock"
	"github.com/metafates/zemi/internal/perm"
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
	// for, so a second process creating the same store retries; only that lock
	// is worth retrying, and anything else keeps the rollback journal the file
	// opens with
	for range 100 {
		if _, err = d.Exec(`PRAGMA journal_mode = WAL`); err == nil || !isBusy(err) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.migrate(); err != nil {
		d.Close()
		return nil, err
	}
	return s, nil
}

// isBusy reports SQLITE_BUSY, the one failure another zemi holding the same
// store causes and a retry answers.
func isBusy(err error) bool {
	e, ok := errors.AsType[*sqlite.Error](err)
	return ok && e.Code()&0xff == sqlite3.SQLITE_BUSY
}

// OpenRead opens an existing store read-only: no migration and no checkpoint
// on close, so a look waits on no lock a running zemi holds.
// A missing file is fs.ErrNotExist; one this binary's migrations have not
// reached is ErrUnmigrated; one SQLite cannot read names that failure.
func OpenRead(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	// query_only makes the read-only promise SQLite's own: a statement that
	// writes fails even when mode=ro would let a temp write through
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"}
	d, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	s := &DB{db: d, path: path}
	if err := s.unmigrated(); err != nil {
		d.Close()
		return nil, fmt.Errorf("%s: %w", perm.TildePath(path), err)
	}
	return s, nil
}

// ErrUnmigrated is a store the embedded migrations have not run on, which a
// listing command cannot read: the remedy is one real open in its project
// (zemi health, or the TUI), which migrates - a read never does.
var ErrUnmigrated = errors.New("store predates this zemi; run zemi health in its project once to migrate it")

// unmigrated reports a store the embedded migrations have not reached: their
// version table is missing (a 0-byte or pre-goose file) or behind their
// count. A file that is not a database at all is its own error: no open
// migrates it, so the unmigrated remedy would never work.
func (s *DB) unmigrated() error {
	var v sql.NullInt64
	err := s.db.QueryRow(`SELECT max(version_id) FROM goose_db_version`).Scan(&v)
	if err == nil {
		if v.Int64 < int64(migrationCount()) {
			return ErrUnmigrated
		}
		return nil
	}
	if isNotADB(err) {
		return fmt.Errorf("not a readable zemi store: %w", err)
	}
	return ErrUnmigrated
}

// isNotADB reports SQLITE_NOTADB: the file is no sqlite database at all, so
// no open recovers it. Other read failures - a hot WAL, a lock - keep the
// unmigrated remedy, which a real open performs.
func isNotADB(err error) bool {
	e, ok := errors.AsType[*sqlite.Error](err)
	return ok && e.Code()&0xff == sqlite3.SQLITE_NOTADB
}

// migrationCount is the newest version the embedded migrations reach; the
// files are numbered 0001.. in step with goose's versions.
func migrationCount() int {
	files, _ := fs.Glob(migrations, "migrations/*.sql")
	return len(files)
}

// ErrInUse is a store another connection holds open: another zemi.
var ErrInUse = errors.New("store: open in another process")

// ClaimForDelete takes a store no other zemi has open, for deleting its
// files, and returns every session id in it, subagent runs too. Leaving WAL
// needs every other connection gone, so another zemi's idle one fails it at
// once (ErrInUse); the exclusive lock holds until release, which runs after
// the files are gone. A missing file, or one SQLite cannot read (garbage, no
// sessions table, or any other read failure), is no ids: its sessions' pads
// age out on their own.
// NOTE: a zemi started there while the files go waits on the lock, then
// writes to the unlinked file and loses it; two windows racing on purpose.
func ClaimForDelete(path string) (sessions []string, release func(), err error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, func() {}, nil
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(0)&_pragma=locking_mode(EXCLUSIVE)"}
	d, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	d.SetMaxOpenConns(1)
	// the one connection keeps the lock between these, since it is the pool's only one
	_, err = d.Exec(`PRAGMA journal_mode = DELETE`)
	if err == nil {
		_, err = d.Exec(`BEGIN EXCLUSIVE`)
	}
	if err == nil {
		rows, qerr := d.Query(`SELECT id FROM sessions`)
		sessions, err = collect(rows, qerr, scanString)
	}
	if isBusy(err) {
		d.Close()
		return nil, nil, ErrInUse
	}
	if err != nil {
		d.Close()
		return nil, func() {}, nil
	}
	return sessions, func() {
		_, _ = d.Exec(`ROLLBACK`)
		d.Close()
	}, nil
}

// SessionCount is how many top-level sessions the store holds: the anchor
// and the subagent runs' child sessions.
func (s *DB) SessionCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM sessions WHERE ` + topLevelSessions).Scan(&n)
	return n, err
}

// Close closes the database.
func (s *DB) Close() error { return s.db.Close() }

// Query runs one statement and returns its rows; a read-only open refuses
// anything that writes.
func (s *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return s.db.Query(query, args...)
}

//go:embed migrations/*.sql
var migrations embed.FS

// migrateLockWait is how long an open waits for another zemi's migration.
const migrateLockWait = 5 * time.Second

// migrate applies the embedded migrations, one goose transaction each, under
// a file lock so two zemis opening one store never run one twice. A store
// made before goose has no version table: the baseline migration is all
// CREATE ... IF NOT EXISTS, so it only stamps the schema already there.
func (s *DB) migrate() error {
	files, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("store: migrations: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, s.db, files, goose.WithLogger(goose.NopLogger()))
	if err != nil {
		return fmt.Errorf("store: %s: %w", s.path, err)
	}
	unlock, err := filelock.Lock(s.path+".lock", migrateLockWait)
	if err != nil {
		return fmt.Errorf("store: migrate %s: %w", s.path, err)
	}
	defer unlock()
	if _, err := p.Up(context.Background()); err != nil {
		return fmt.Errorf("store: migrate %s: %w", s.path, err)
	}
	return nil
}

// newID returns a random 128-bit id, hex encoded, for rows nobody types.
func newID() string {
	var b [16]byte
	rand.Read(b[:]) // crashes the process rather than fail
	return hex.EncodeToString(b[:])
}

// shortEnc is lowercase Crockford base32: no i, l, o or u, the same name on a
// case-folding disk, and never a leading '-'.
var shortEnc = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// IDShape is what ShortID makes, the one check for a file name read back as an id.
var IDShape = regexp.MustCompile(`^[0-9a-hjkmnp-tv-z]{8}$`)

// ShortID returns a random 40-bit id in 8 characters, for the ids people and
// the model read and type: sessions, agent and workflow runs, memories.
// NOTE: a collision is a refused primary key insert, about n^2/2^41 over n rows
// of one table (1e-3 at 50k); widen to 10 characters if stores grow past that.
func ShortID() string {
	var b [5]byte
	rand.Read(b[:])
	return shortEnc.EncodeToString(b[:])
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
	b, _ := json.Marshal(s, json.Deterministic(true))
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

// one scans a single row; no row is ErrNotFound.
func one[T any](row *sql.Row, scan func(scanner) (T, error)) (T, error) {
	v, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return v, err
}

func scanString(r scanner) (s string, err error) {
	err = r.Scan(&s)
	return s, err
}

// changed runs a write and reports whether it touched a row.
func (s *DB) changed(query string, args ...any) (bool, error) {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// execOne runs an update or delete of one row by key; no match is ErrNotFound.
func (s *DB) execOne(what, query string, args ...any) error {
	ok, err := s.changed(query, args...)
	if err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// inTx runs fn in one transaction, committed when fn returns nil.
func (s *DB) inTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
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
// it names the root already, touches: its mtime is the last use.
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
		if !project.Touch() {
			_ = os.WriteFile(filepath.Join(filepath.Dir(project.path), "root"), []byte(projectRoot+"\n"), 0o600)
		}
	}
	return global, project, nil
}

// Touch marks a project store used now, best effort: its root file's mtime
// is the project's last use. It reports whether the store has a root file.
func (s *DB) Touch() bool {
	dir := filepath.Dir(s.path)
	if _, _, ok := ProjectRoot(dir); !ok {
		return false
	}
	t := time.Now()
	_ = os.Chtimes(filepath.Join(dir, "root"), t, t)
	return true
}

// ProjectRoot reads the root file of a project state dir: the trust root the
// dir belongs to and when zemi last used it. It is ok only when the root
// hashes to the dir's name, so a copied or edited file names nothing.
func ProjectRoot(dir string) (root string, used time.Time, ok bool) {
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
