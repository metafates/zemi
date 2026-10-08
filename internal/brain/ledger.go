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

// OpKind is what one memory_ops row did (DESIGN §7).
type OpKind string

// Op kinds (DESIGN §7).
const (
	OpAdd       OpKind = "add"
	OpSupersede OpKind = "supersede"
	OpForget    OpKind = "forget"
	OpEdit      OpKind = "edit"
	OpWrite     OpKind = "write"
	OpMoveOut   OpKind = "move-out"
	OpMoveIn    OpKind = "move-in"
	OpArchive   OpKind = "archive"
	OpImport    OpKind = "import"
)

// ContentCap caps a brain file's content and the prior bytes a write op
// keeps for rewind (DESIGN §7, §10.4).
const ContentCap = 32 << 10

// Op is one memory_ops row (DESIGN §7): INSERT-only, never compacted. Line ops
// (add, supersede, forget, edit, archive, move, import) carry the line's
// text, hash and origin; write ops carry the replaced brain file's prior
// bytes. Import entries are the permanent tombstones.
type Op struct {
	Op           OpKind
	ID           string // the memory id; the brain file's path for a write op
	TS           int64  // millis; zero means now
	Text         string // the line text this op wrote or removed
	PriorText    string // edit: the text this replaced
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

// Ledger is a partition's memory_ops trail, in the store the scope already
// uses. brain opens its own handle on the same file - store.DB's is
// unexported - which SQLite
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
	// the store's schema holds memory_ops; a query answers whether this handle
	// opened a database at all, which a garbage file is not
	var version int
	if err := d.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		d.Close()
		return nil, fmt.Errorf("brain: ledger in %s: %w", p.DB, err)
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
// that follows it under the same lock (DESIGN §7).
func (l *Ledger) Insert(op *Op) error {
	if l.db == nil {
		return errors.New("brain: no store holds the ledger")
	}
	// an anonymous row is unlintable and unrestorable (DESIGN §7, §10.7)
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

// ByID is the id's trail, oldest first.
func (l *Ledger) ByID(id string) ([]*Op, error) {
	if l.db == nil {
		return nil, nil
	}
	rows, err := l.db.Query(`SELECT `+opCols+` FROM memory_ops WHERE id = ? ORDER BY rowid`, id)
	return collectOps(rows, err, true)
}

// Latest is the newest op per id, the state /memory and the reflector
// read. Nothing reads the ledger to build the prompt (DESIGN §7).
func (l *Ledger) Latest() ([]*Op, error) {
	if l.db == nil {
		return nil, nil
	}
	rows, err := l.db.Query(`SELECT ` + opCols + ` FROM memory_ops
		WHERE rowid IN (SELECT MAX(rowid) FROM memory_ops GROUP BY id)
		ORDER BY ts DESC, rowid DESC`)
	return collectOps(rows, err, true)
}

// Session is the ops session sid wrote between from and until (millis),
// oldest first: rewind's forgetLearned replays them (DESIGN §7). Import
// entries match on the frozen row's created_at, their op ts being the
// migration moment, outside any rewound range.
func (l *Ledger) Session(sid string, from, until int64) ([]*Op, error) {
	if l.db == nil {
		return nil, nil
	}
	rows, err := l.db.Query(`SELECT `+opCols+` FROM memory_ops
		WHERE session_id = ? AND (ts BETWEEN ? AND ? OR op = ? AND created_at BETWEEN ? AND ?)
		ORDER BY rowid`, sid, from, until, OpImport, from, until)
	return collectOps(rows, err, true)
}

// ops is every entry, oldest first; lint replays it (DESIGN §10.7). The write
// ops' prior bytes stay unread: only rewind's Session read restores them.
func (l *Ledger) ops() ([]*Op, error) {
	if l.db == nil {
		return nil, nil
	}
	rows, err := l.db.Query(`SELECT ` + opColsNoPrior + ` FROM memory_ops ORDER BY rowid`)
	return collectOps(rows, err, false)
}

// Stamp is what display needs about one id (DESIGN §7): First is the first writing
// op's ts (the line's age), Quote the latest quote the trail carried,
// Session the first writer's provenance.
type Stamp struct {
	First   int64
	Quote   string
	Session string
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
		if op.Quote != "" {
			s.Quote = op.Quote
		}
		out[op.ID] = s
	}
	return out, nil
}

const opCols = `op, id, ts, text, prior_text, quote, session_id, hash, superseded_by, scope, origin,
	prior_bytes, prior_truncated, prior_existed, created_at, updated_at`

// opColsNoPrior is opCols without the write ops' prior bytes: lint, Stamps and
// Reconcile read every entry and none of them restores a file, while the table
// is append-only and a write op carries up to ContentCap of them.
const opColsNoPrior = `op, id, ts, text, prior_text, quote, session_id, hash, superseded_by, scope, origin,
	created_at, updated_at`

// collectOps scans a query's rows; prior says whether its projection carried
// the write ops' prior bytes.
func collectOps(rows *sql.Rows, err error, prior bool) ([]*Op, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Op
	for rows.Next() {
		op := &Op{}
		var scope, origin string
		dest := []any{&op.Op, &op.ID, &op.TS, &op.Text, &op.PriorText, &op.Quote,
			&op.SessionID, &op.Hash, &op.SupersededBy, &scope, &origin}
		if prior {
			dest = append(dest, &op.Prior, &op.Truncated, &op.Existed)
		}
		dest = append(dest, &op.CreatedAt, &op.UpdatedAt)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		op.Scope, op.Origin = store.Scope(scope), store.Origin(origin)
		out = append(out, op)
	}
	return out, rows.Err()
}
