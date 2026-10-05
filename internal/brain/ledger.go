package brain

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	_ "modernc.org/sqlite" // the "sqlite" driver store registers; kept explicit

	"github.com/metafates/zemi/internal/store"
)

// Op kinds (D7).
const (
	OpAdd       = "add"
	OpSupersede = "supersede"
	OpForget    = "forget"
	OpEdit      = "edit"
	OpWrite     = "write"
	OpMoveOut   = "move-out"
	OpMoveIn    = "move-in"
	OpArchive   = "archive"
	OpImport    = "import"
)

// ContentCap caps a brain file's content and the prior bytes a write op
// keeps for rewind (D5, D7); hostapi's contextBudget is the same number.
const ContentCap = 32 << 10

// Op is one memory_ops row (D7): INSERT-only, never compacted. Line ops
// (add, supersede, forget, edit, archive, move, import) carry the line's
// text, hash and origin; write ops carry the replaced brain file's prior
// bytes. Import entries are the permanent tombstones.
type Op struct {
	Op           string
	ID           string // the memory id; the brain file's path for a write op
	TS           int64  // millis; zero means now
	Text         string // the line text this op wrote or removed
	PriorText    string // edit and supersede: what this replaced
	Quote        string
	SessionID    string // provenance; the acting session of an archive
	Hash         string // sha256 of Text, on memories.md line ops
	SupersededBy string // supersede: the replacing id
	Scope        store.Scope
	Origin       store.Origin
	Prior        []byte // write ops: the replaced file's bytes, ContentCap-capped
	Truncated    bool   // Prior was over ContentCap
	Existed      bool   // write ops: the file existed (a creation has no prior)
	CreatedAt    int64  // import: the frozen row's stamps
	UpdatedAt    int64
}

// opsSchema is created lazily in the store the scope already uses (D1).
// No schema bump: the table is IF NOT EXISTS and user_version untouched.
const opsSchema = `
CREATE TABLE IF NOT EXISTS memory_ops (
	op TEXT NOT NULL,
	id TEXT NOT NULL,
	ts INTEGER NOT NULL,
	text TEXT NOT NULL DEFAULT '',
	prior_text TEXT NOT NULL DEFAULT '',
	quote TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	hash TEXT NOT NULL DEFAULT '',
	superseded_by TEXT NOT NULL DEFAULT '',
	scope TEXT NOT NULL DEFAULT '',
	origin TEXT NOT NULL DEFAULT '',
	prior_bytes BLOB,
	prior_truncated INTEGER NOT NULL DEFAULT 0,
	prior_existed INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS memory_ops_id ON memory_ops(id);
`

// Ledger is a partition's memory_ops trail, in the store the scope already
// uses. brain opens its own handle on the same file - store.DB's is
// unexported and this stage must not change store - which SQLite
// serializes like a second process, on the WAL and busy_timeout both keep.
type Ledger struct {
	db *sql.DB
}

// OpenLedger opens the partition's ledger. A store that is not there yet
// is an empty ledger - reads are empty, writes fail - since zemi never
// creates a bare DB its own open would refuse.
func (p Partition) OpenLedger() (*Ledger, error) {
	if p.DB == "" {
		return &Ledger{}, nil
	}
	if _, err := os.Stat(p.DB); errors.Is(err, fs.ErrNotExist) {
		return &Ledger{}, nil
	}
	d, err := sql.Open("sqlite", p.DB+"?_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("brain: open %s: %w", p.DB, err)
	}
	d.SetMaxOpenConns(1)
	if _, err := d.Exec(opsSchema); err != nil {
		d.Close()
		return nil, fmt.Errorf("brain: ledger schema in %s: %w", p.DB, err)
	}
	return &Ledger{db: d}, nil
}

// Close releases the handle; an empty ledger closes as nothing.
func (l *Ledger) Close() error {
	if l.db == nil {
		return nil
	}
	return l.db.Close()
}

// Insert appends one op, the ledger entry preceding every file replace
// that follows it under the same lock (D1).
func (l *Ledger) Insert(op *Op) error {
	if l.db == nil {
		return errors.New("brain: no store holds the ledger")
	}
	// an anonymous row is unlintable and unrestorable (D6, D7)
	if op.Op == "" {
		return errors.New("brain: ledger insert without an op kind")
	}
	if op.TS == 0 {
		op.TS = time.Now().UnixMilli()
	}
	_, err := l.db.Exec(`INSERT INTO memory_ops
		(op, id, ts, text, prior_text, quote, session_id, hash, superseded_by, scope, origin,
		 prior_bytes, prior_truncated, prior_existed, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		op.Op, op.ID, op.TS, op.Text, op.PriorText, op.Quote, op.SessionID, op.Hash,
		op.SupersededBy, op.Scope, op.Origin, op.Prior, op.Truncated, op.Existed,
		op.CreatedAt, op.UpdatedAt)
	if err != nil {
		return fmt.Errorf("brain: ledger insert: %w", err)
	}
	return nil
}

// Import appends an import entry unless the ledger already holds the id:
// the tombstone is what keeps a hand-deleted line from re-importing (D1),
// so replay is idempotent. Reports whether it inserted. Migration callers
// hold the partition lock (D1), which makes the check and the insert one
// step; a lock-free duplicate would write a harmless second tombstone.
func (l *Ledger) Import(op *Op) (bool, error) {
	if l.db == nil {
		return false, errors.New("brain: no store holds the ledger")
	}
	var have bool
	if err := l.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM memory_ops WHERE id = ?)`, op.ID).Scan(&have); err != nil {
		return false, err
	}
	if have {
		return false, nil
	}
	op.Op = OpImport
	return true, l.Insert(op)
}

// ByID is the id's trail, oldest first.
func (l *Ledger) ByID(id string) ([]*Op, error) {
	if l.db == nil {
		return nil, nil
	}
	rows, err := l.db.Query(`SELECT `+opCols+` FROM memory_ops WHERE id = ? ORDER BY rowid`, id)
	return collectOps(rows, err)
}

// Latest is the newest op per id, the state /memory and the reflector
// read. Nothing reads the ledger to build the prompt (D1).
func (l *Ledger) Latest() ([]*Op, error) {
	if l.db == nil {
		return nil, nil
	}
	rows, err := l.db.Query(`SELECT ` + opCols + ` FROM memory_ops
		WHERE rowid IN (SELECT MAX(rowid) FROM memory_ops GROUP BY id)
		ORDER BY ts DESC, rowid DESC`)
	return collectOps(rows, err)
}

// Session is the ops session sid wrote between from and until (millis),
// oldest first: rewind's forgetLearned replays them (section 7).
func (l *Ledger) Session(sid string, from, until int64) ([]*Op, error) {
	if l.db == nil {
		return nil, nil
	}
	rows, err := l.db.Query(`SELECT `+opCols+` FROM memory_ops
		WHERE session_id = ? AND ts BETWEEN ? AND ? ORDER BY rowid`, sid, from, until)
	return collectOps(rows, err)
}

// ops is every entry, oldest first; lint replays it (D6).
func (l *Ledger) ops() ([]*Op, error) {
	if l.db == nil {
		return nil, nil
	}
	rows, err := l.db.Query(`SELECT ` + opCols + ` FROM memory_ops ORDER BY rowid`)
	return collectOps(rows, err)
}

// Stamp is what display needs about one id (D7): First and Last are the
// first and latest writing op's ts (the line's age), Quote the latest
// quote the trail carried, Session the first writer's provenance.
type Stamp struct {
	First, Last int64
	Quote       string
	Session     string
}

// Stamps folds the ledger to one Stamp per id, for /memory, the reflector's
// index and the overlay's trail, which read quote and age beside the lines.
func (l *Ledger) Stamps() (map[string]Stamp, error) {
	ops, err := l.ops()
	if err != nil {
		return nil, err
	}
	out := make(map[string]Stamp, len(ops))
	for _, op := range ops {
		s := out[op.ID]
		if s.First == 0 && (op.Op == OpAdd || op.Op == OpImport || op.Op == OpMoveIn) {
			s.First, s.Session = op.TS, op.SessionID
		}
		switch op.Op {
		case OpAdd, OpEdit, OpImport, OpMoveIn:
			s.Last = op.TS
		}
		if op.Quote != "" {
			s.Quote = op.Quote
		}
		out[op.ID] = s
	}
	return out, nil
}

const opCols = `op, id, ts, text, prior_text, quote, session_id, hash, superseded_by, scope, origin,
	prior_bytes, prior_truncated, prior_existed, created_at, updated_at`

func collectOps(rows *sql.Rows, err error) ([]*Op, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Op
	for rows.Next() {
		op := &Op{}
		var scope, origin string
		if err := rows.Scan(&op.Op, &op.ID, &op.TS, &op.Text, &op.PriorText, &op.Quote,
			&op.SessionID, &op.Hash, &op.SupersededBy, &scope, &origin,
			&op.Prior, &op.Truncated, &op.Existed, &op.CreatedAt, &op.UpdatedAt); err != nil {
			return nil, err
		}
		op.Scope, op.Origin = store.Scope(scope), store.Origin(origin)
		out = append(out, op)
	}
	return out, rows.Err()
}
