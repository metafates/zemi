package store

import (
	"fmt"
	"slices"
)

// Checkpoint is a file as it was before a turn first changed it.
type Checkpoint struct {
	Path    string
	Existed bool
	Content []byte
}

// AddCheckpoint records path's content before the turn started by the user
// message messageID changed it; later snapshots of the same path in that
// turn are ignored, so the row always holds the pre-turn state.
func (s *DB) AddCheckpoint(sessionID, messageID, path string, existed bool, content []byte) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO checkpoints
		(message_id, session_id, path, existed, content, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		messageID, sessionID, path, existed, content, now())
	if err != nil {
		return fmt.Errorf("store: add checkpoint: %w", err)
	}
	return nil
}

// CheckpointsFrom returns, for every file changed in the turns from
// messageID on, its content before the earliest of those turns.
func (s *DB) CheckpointsFrom(sessionID, messageID string) ([]Checkpoint, error) {
	rows, err := s.db.Query(`SELECT c.path, c.existed, c.content FROM checkpoints c
		JOIN messages m ON m.id = c.message_id
		WHERE c.session_id = ? AND m.rowid >= (SELECT rowid FROM messages WHERE id = ?)
		ORDER BY m.rowid, c.created_at`, sessionID, messageID)
	all, err := collect(rows, err, func(r scanner) (cp Checkpoint, err error) {
		err = r.Scan(&cp.Path, &cp.Existed, &cp.Content)
		return
	})
	seen := map[string]bool{}
	return slices.DeleteFunc(all, func(cp Checkpoint) bool {
		dup := seen[cp.Path]
		seen[cp.Path] = true
		return dup
	}), err
}

// CheckpointCounts maps each user message id of a session to the number of
// files its turn changed.
func (s *DB) CheckpointCounts(sessionID string) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT message_id, COUNT(*) FROM checkpoints WHERE session_id = ? GROUP BY message_id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// TruncateMessages deletes messageID and every later message of the session
// (their checkpoints and tool calls cascade) and drops a summary pointer
// left dangling by the cut.
func (s *DB) TruncateMessages(sessionID, messageID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var rowid int64
	if err := tx.QueryRow(`SELECT rowid FROM messages WHERE id = ? AND session_id = ?`, messageID, sessionID).Scan(&rowid); err != nil {
		return ErrNotFound
	}
	if _, err := tx.Exec(`DELETE FROM messages WHERE session_id = ? AND rowid >= ?`, sessionID, rowid); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE sessions SET summary_message_id = NULL, updated_at = ? WHERE id = ?
		AND summary_message_id IS NOT NULL
		AND summary_message_id NOT IN (SELECT id FROM messages WHERE session_id = ?)`, now(), sessionID, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}
