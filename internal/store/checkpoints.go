package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/metafates/zemi/internal/perm"
)

// Checkpoint is a file as it was before a turn first changed it.
type Checkpoint struct {
	Path    string
	Existed bool
	// Link means Content is the target of a symbolic link
	Link    bool
	Content []byte
}

// AddCheckpoint records cp.Path's content before the turn started by the user
// message messageID changed it; later snapshots of the same path in that
// turn are ignored, so the row always holds the pre-turn state.
func (s *DB) AddCheckpoint(sessionID, messageID string, cp Checkpoint) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO checkpoints
		(message_id, session_id, path, existed, link, content, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		messageID, sessionID, cp.Path, cp.Existed, cp.Link, cp.Content, now())
	if err != nil {
		return fmt.Errorf("store: add checkpoint: %w", err)
	}
	return nil
}

// CheckpointsFrom returns, for every file changed in the turns from
// messageID on, its content before the earliest of those turns.
func (s *DB) CheckpointsFrom(sessionID, messageID string) ([]Checkpoint, error) {
	rows, err := s.db.Query(`SELECT c.path, c.existed, c.link, c.content FROM checkpoints c
		JOIN messages m ON m.id = c.message_id
		WHERE c.session_id = ? AND m.rowid >= (SELECT rowid FROM messages WHERE id = ?)
		ORDER BY m.rowid, c.created_at`, sessionID, messageID)
	all, err := collect(rows, err, func(r scanner) (cp Checkpoint, err error) {
		err = r.Scan(&cp.Path, &cp.Existed, &cp.Link, &cp.Content)
		return cp, err
	})
	seen := map[string]bool{}
	return slices.DeleteFunc(all, func(cp Checkpoint) bool {
		// NOTE: folds like the file tools' lock key, so two files differing
		// only in case on a case-sensitive volume restore just the earlier one
		key := perm.FoldCase(cp.Path)
		dup := seen[key]
		seen[key] = true
		return dup
	}), err
}

// CheckpointPaths maps each user message id of a session to the paths its
// turn snapshotted.
func (s *DB) CheckpointPaths(sessionID string) (map[string][]string, error) {
	rows, err := s.db.Query(`SELECT message_id, path FROM checkpoints WHERE session_id = ?`, sessionID)
	out := map[string][]string{}
	_, err = collect(rows, err, func(r scanner) (id string, err error) {
		var path string
		if err = r.Scan(&id, &path); err == nil {
			out[id] = append(out[id], path)
		}
		return id, err
	})
	return out, err
}

// TruncateMessages deletes messageID and every later message of the session
// (their checkpoints and tool calls cascade).
func (s *DB) TruncateMessages(sessionID, messageID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var rowid int64
	err = tx.QueryRow(`SELECT rowid FROM messages WHERE id = ? AND session_id = ?`, messageID, sessionID).Scan(&rowid)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: truncate messages: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM messages WHERE session_id = ? AND rowid >= ?`, sessionID, rowid); err != nil {
		return err
	}
	return tx.Commit()
}
