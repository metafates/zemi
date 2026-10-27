// checkpoints.go stores a file as it was before a turn first changed it,
// keyed by the log seq of the turn's user message: the rewind to that
// message restores every file changed from it on.

package store

import (
	"fmt"
	"slices"

	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/set"
)

// Checkpoint is a file as it was before a turn first changed it.
type Checkpoint struct {
	Path    string
	Existed bool
	// Link means Content is the target of a symbolic link
	Link    bool
	Content []byte
}

// AddCheckpoint records cp.Path's content before the turn that starts at seq
// changed it; later snapshots of the same path in that turn are ignored, so
// the row always holds the pre-turn state.
func (s *DB) AddCheckpoint(seq int64, cp Checkpoint) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO checkpoints
		(seq, path, existed, link, content, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		seq, cp.Path, cp.Existed, cp.Link, cp.Content, now())
	if err != nil {
		return fmt.Errorf("store: add checkpoint: %w", err)
	}
	return nil
}

// CheckpointsFrom returns, for every file changed in the turns from seq on,
// its content before the earliest of those turns.
func (s *DB) CheckpointsFrom(seq int64) ([]Checkpoint, error) {
	rows, err := s.db.Query(`SELECT path, existed, link, content FROM checkpoints
		WHERE seq >= ? ORDER BY seq, created_at`, seq)
	all, err := collect(rows, err, func(r scanner) (cp Checkpoint, err error) {
		err = r.Scan(&cp.Path, &cp.Existed, &cp.Link, &cp.Content)
		return cp, err
	})
	seen := set.Set[string]{}
	return slices.DeleteFunc(all, func(cp Checkpoint) bool {
		// NOTE: folds like the file tools' lock key, so two files differing
		// only in case on a case-sensitive volume restore just the earlier one
		key := perm.FoldCase(cp.Path)
		dup := seen.Has(key)
		seen.Add(key)
		return dup
	}), err
}

// CheckpointPaths maps the seq of a turn's first message to the paths its
// turn snapshotted, for the rewind picker.
func (s *DB) CheckpointPaths() (map[int64][]string, error) {
	rows, err := s.db.Query(`SELECT seq, path FROM checkpoints`)
	out := map[int64][]string{}
	_, err = collect(rows, err, func(r scanner) (at struct{}, err error) {
		var seq int64
		var path string
		if err = r.Scan(&seq, &path); err == nil {
			out[seq] = append(out[seq], path)
		}
		return at, err
	})
	return out, err
}
