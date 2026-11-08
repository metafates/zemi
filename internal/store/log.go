// log.go is the eternal chat's storage: the append-only log, the binary tree
// of summary nodes built over it, and the persisted view. One main log per
// project DB; seq is the 0-based message index and stays dense by
// construction: a reset empties the log and the next append starts at 0.

package store

import (
	"database/sql"
	"fmt"
	"time"
)

// LogEntry is one row of the log: one message, word for word. Kind is one of
// the row kinds below.
type LogEntry struct {
	Seq       int64
	Kind      LogKind
	Text      string
	MetaJSON  string // "" when none
	PartsJSON string // "" when none
	CreatedAt string
	Origin    string // the window that wrote it; '' predates the column
}

// LogKind is a log row's kind: the word stored in the log table's kind
// column and compared against in its queries.
type LogKind string

// The row kinds.
const (
	KindUser    LogKind = "user"
	KindAgent   LogKind = "agent"
	KindTool    LogKind = "tool"
	KindEcho    LogKind = "echo"
	KindWork    LogKind = "work"
	KindNote    LogKind = "note"
	KindThought LogKind = "thought"
	KindError   LogKind = "error"
	KindShell   LogKind = "shell"
	KindNotice  LogKind = "notice"
)

// Node is one built summary: node(L,I) covers the 2^L messages from I*2^L on
// in one line of Size UTF-8 bytes.
type Node struct {
	L, I    int64
	Text    string
	Size    int
	BuiltAt string
}

// NodeRef is a built node without its text, what queue and view accounting
// read.
type NodeRef struct {
	L, I int64
	Size int
}

// ViewLine is one line of the persisted view: node(L,I), oldest first by its
// ord, the node's first message. A line whose node is not built yet renders
// as a placeholder.
type ViewLine struct {
	L, I int64
}

// ViewMerge is one view merge: the pair of sibling lines under (L,I) -
// node(L-1,2I) and node(L-1,2I+1) - becomes their parent (L,I).
type ViewMerge struct {
	L, I int64
}

// AppendLog appends one chat row and its view line in one transaction and
// returns the row's seq: max+1 inside the transaction, so a tail cut leaves
// the index dense. When nodeText is not "", the message's own node lands
// built in the same transaction (template-built leaves), so everything a
// reader needs commits at once.
func (s *DB) AppendLog(kind LogKind, text, metaJSON, partsJSON, nodeText string) (int64, error) {
	var seq int64
	err := s.inTx(func(tx *sql.Tx) error {
		err := tx.QueryRow(`INSERT INTO log (seq, kind, text, meta_json, parts_json, created_at, origin)
			VALUES ((SELECT COALESCE(MAX(seq), -1) + 1 FROM log), ?, ?, ?, ?, ?, ?)
			RETURNING seq`,
			kind, text, nullString(metaJSON), nullString(partsJSON), nowStamp(), s.Origin).Scan(&seq)
		if err != nil {
			return fmt.Errorf("store: append log: %w", err)
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO view_lines (ord, l, i) VALUES (?, 0, ?)`, seq, seq); err != nil {
			return fmt.Errorf("store: append view line: %w", err)
		}
		if nodeText != "" {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO nodes (l, i, text, size, built_at) VALUES (0, ?, ?, ?, ?)`,
				seq, nodeText, len(nodeText), nowStamp()); err != nil {
				return fmt.Errorf("store: append node: %w", err)
			}
		}
		return nil
	})
	return seq, err
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

const _logCols = `seq, kind, text, COALESCE(meta_json, ''), COALESCE(parts_json, ''), created_at, COALESCE(origin, '')`

func scanLog(r scanner) (e LogEntry, err error) {
	err = r.Scan(&e.Seq, &e.Kind, &e.Text, &e.MetaJSON, &e.PartsJSON, &e.CreatedAt, &e.Origin)
	return e, err
}

// LogFrom reads the log from seq on, oldest first, at most limit rows.
func (s *DB) LogFrom(seq int64, limit int) ([]LogEntry, error) {
	rows, err := s.db.Query(`SELECT `+_logCols+` FROM log WHERE seq >= ? ORDER BY seq LIMIT ?`, seq, limit)
	return collect(rows, err, scanLog)
}

// LogTail reads the last n rows, oldest first.
func (s *DB) LogTail(n int) ([]LogEntry, error) {
	rows, err := s.db.Query(`SELECT * FROM (SELECT `+_logCols+` FROM log ORDER BY seq DESC LIMIT ?) ORDER BY seq`, n)
	return collect(rows, err, scanLog)
}

// LogKindBack reads the last n rows of one kind, oldest first: the composer
// history's user rows.
func (s *DB) LogKindBack(kind LogKind, limit int) ([]LogEntry, error) {
	rows, err := s.db.Query(`SELECT * FROM (SELECT `+_logCols+` FROM log WHERE kind = ? ORDER BY seq DESC LIMIT ?) ORDER BY seq`, kind, limit)
	return collect(rows, err, scanLog)
}

// LogAt reads one row; ErrNotFound when absent.
func (s *DB) LogAt(seq int64) (LogEntry, error) {
	return one(s.db.QueryRow(`SELECT `+_logCols+` FROM log WHERE seq = ?`, seq), scanLog)
}

// LogCount is how many rows the log holds. An append takes max+1 and only a
// reset deletes rows, so the seqs stay dense and the count is the max: one
// seek down the index, where COUNT(*) walks the table.
func (s *DB) LogCount() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(seq), -1) + 1 FROM log`).Scan(&n)
	return n, err
}

// SetLogMeta fills one log row's meta_json after the append: the turn stat
// its reply row gains when the turn ends. The text and the seq never move.
// The write merges over the meta the row holds: a split reply's last sibling
// carries the fold mark there, and a wholesale swap would drop it and redraw
// one message as many rows.
func (s *DB) SetLogMeta(seq int64, metaJSON string) error {
	if _, err := s.db.Exec(`UPDATE log SET meta_json = json_patch(COALESCE(meta_json, '{}'), ?) WHERE seq = ?`, metaJSON, seq); err != nil {
		return fmt.Errorf("store: set log meta: %w", err)
	}
	return nil
}

// CVKey is the kv row the builder's compaction view persists in: a JSON
// array of [l, i] pairs.
const CVKey = "chatlog.cv"

// ResetLog wipes the chat: the log rows, every summary node and view line,
// and the compaction view. The next append starts at seq 0 again. VACUUM and
// a TRUNCATE wal_checkpoint follow: without them the wiped rows would stay
// in the file's free pages, and a wiped chat would be readable there.
func (s *DB) ResetLog() error {
	err := s.inTx(func(tx *sql.Tx) error {
		for _, table := range []string{"log", "nodes", "view_lines"} {
			if _, err := tx.Exec(`DELETE FROM ` + table); err != nil {
				return fmt.Errorf("store: reset %s: %w", table, err)
			}
		}
		if _, err := tx.Exec(`DELETE FROM kv WHERE key = ?`, CVKey); err != nil {
			return fmt.Errorf("store: reset compaction view: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("store: vacuum: %w", err)
	}
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("store: checkpoint: %w", err)
	}
	return nil
}

// InsertNode stores a built summary, reporting whether this call inserted it:
// INSERT OR IGNORE dedupes concurrent builders, and a builder that lost the
// race reads the winner back instead of rebuilding. A node whose range runs
// past the log is refused: its build raced a reset's wipe, and the seqs it
// names are gone or reused.
func (s *DB) InsertNode(l, i int64, text string) (bool, error) {
	var msgs int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(seq), -1) + 1 FROM log`).Scan(&msgs); err != nil {
		return false, fmt.Errorf("store: insert node: %w", err)
	}
	if (i+1)*(1<<l) > msgs {
		return false, nil
	}
	ok, err := s.changed(`INSERT OR IGNORE INTO nodes (l, i, text, size, built_at) VALUES (?, ?, ?, ?, ?)`,
		l, i, text, len(text), nowStamp())
	if err != nil {
		return false, fmt.Errorf("store: insert node: %w", err)
	}
	return ok, nil
}

// NodeAt reads one built summary; ErrNotFound when it is not built yet.
func (s *DB) NodeAt(l, i int64) (Node, error) {
	return one(s.db.QueryRow(`SELECT l, i, text, size, built_at FROM nodes WHERE l = ? AND i = ?`, l, i),
		func(r scanner) (n Node, err error) {
			err = r.Scan(&n.L, &n.I, &n.Text, &n.Size, &n.BuiltAt)
			return n, err
		})
}

// BuiltNodes lists every built summary with its size, level then position.
func (s *DB) BuiltNodes() ([]NodeRef, error) {
	rows, err := s.db.Query(`SELECT l, i, size FROM nodes ORDER BY l, i`)
	return collect(rows, err, func(r scanner) (n NodeRef, err error) {
		err = r.Scan(&n.L, &n.I, &n.Size)
		return n, err
	})
}

// MergeViewLines applies merges in one transaction: each runs only while both
// its children are still there, so two windows merging from stale views can
// never double-cover a message - a merge another window already made is
// skipped, not replayed. It returns how many ran.
func (s *DB) MergeViewLines(merges []ViewMerge) (int, error) {
	if len(merges) == 0 {
		return 0, nil
	}
	applied := 0
	err := s.inTx(func(tx *sql.Tx) error {
		for _, m := range merges {
			// the parent (L,I) sits where its first child sat; the second
			// child's line is the one that goes. The children are matched by
			// node, not ord alone: a row another merge left at that ord must
			// not stand in for a child.
			ord1, ord2 := m.I<<m.L, m.I<<m.L+1<<(m.L-1)
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM view_lines WHERE
				(ord = ? AND l = ? AND i = ?) OR (ord = ? AND l = ? AND i = ?)`,
				ord1, m.L-1, m.I*2, ord2, m.L-1, m.I*2+1).Scan(&n); err != nil {
				return err
			}
			if n < 2 {
				continue
			}
			if _, err := tx.Exec(`INSERT OR REPLACE INTO view_lines (ord, l, i) VALUES (?, ?, ?)`, ord1, m.L, m.I); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM view_lines WHERE ord = ?`, ord2); err != nil {
				return err
			}
			applied++
		}
		return nil
	})
	return applied, err
}

// LoadView reads the persisted view, oldest first.
func (s *DB) LoadView() ([]ViewLine, error) {
	rows, err := s.db.Query(`SELECT l, i FROM view_lines ORDER BY ord`)
	return collect(rows, err, func(r scanner) (v ViewLine, err error) {
		err = r.Scan(&v.L, &v.I)
		return v, err
	})
}

// ViewLineText is one view line with its node's text and byte size, the
// placeholder's when the node is not built yet (Text empty).
type ViewLineText struct {
	L, I int64
	Text string
	Size int
}

// ViewLines reads the persisted view with each line's node text in one
// query, oldest first: what the view message and the /memories browser draw.
func (s *DB) ViewLines() ([]ViewLineText, error) {
	rows, err := s.db.Query(`SELECT v.l, v.i, COALESCE(n.text, ''), COALESCE(n.size, 0)
		FROM view_lines v LEFT JOIN nodes n ON n.l = v.l AND n.i = v.i ORDER BY v.ord`)
	return collect(rows, err, func(r scanner) (v ViewLineText, err error) {
		err = r.Scan(&v.L, &v.I, &v.Text, &v.Size)
		return v, err
	})
}

// MainSessionID is the anchor session row's id: billing and todos ride it
// once the chat is the memory.
const MainSessionID = "main"
