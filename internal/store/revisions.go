// revisions.go implements the config_revisions table (DESIGN §7): one row
// per applied or attempted change to the config repo, with the paths it
// wrote and its outcome. Revisions are user-scope: they live in the
// global DB, and the DB mark lands before the git commit (fail-closed on
// a crash between the two, §10.4).
package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// ConfigRevision is one recorded config-repo change. Status is pending,
// applied, failed or reverted; Commit is the config-repo commit SHA once
// the git commit landed.
type ConfigRevision struct {
	ID          string
	CreatedAt   int64
	Title       string
	ChangesJSON string
	PathsJSON   string
	Status      string
	AppliedBy   string
	Commit      string
}

const revisionCols = `id, created_at, title, changes_json, paths_json, status, applied_by, commit_sha`

func scanRevision(row scanner) (*ConfigRevision, error) {
	var r ConfigRevision
	return &r, row.Scan(&r.ID, &r.CreatedAt, &r.Title, &r.ChangesJSON,
		&r.PathsJSON, &r.Status, &r.AppliedBy, &r.Commit)
}

// AddRevision records a new revision (status pending until its commit
// lands).
func (s *DB) AddRevision(r *ConfigRevision) error {
	if r.ID == "" {
		return fmt.Errorf("store: add revision: id is required")
	}
	if r.CreatedAt == 0 {
		r.CreatedAt = now()
	}
	if r.Status == "" {
		r.Status = "pending"
	}
	_, err := s.db.Exec(`INSERT INTO config_revisions
		(id, created_at, title, changes_json, paths_json, status, applied_by, commit_sha)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.CreatedAt, r.Title, r.ChangesJSON, r.PathsJSON, r.Status, r.AppliedBy, r.Commit)
	if err != nil {
		return fmt.Errorf("store: add revision: %w", err)
	}
	return nil
}

// GetRevision returns one revision by id.
func (s *DB) GetRevision(id string) (*ConfigRevision, error) {
	row := s.db.QueryRow(`SELECT `+revisionCols+` FROM config_revisions WHERE id = ?`, id)
	r, err := scanRevision(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// ListRevisions returns revisions newest first.
func (s *DB) ListRevisions() ([]*ConfigRevision, error) {
	rows, err := s.db.Query(`SELECT ` + revisionCols + ` FROM config_revisions ORDER BY created_at DESC, id`)
	return collect(rows, err, scanRevision)
}

// SetRevisionCommit records the config-repo commit SHA of an applied
// revision (what /config revert reverts).
func (s *DB) SetRevisionCommit(id, commit string) error {
	return s.execOne("set revision commit", `UPDATE config_revisions SET commit_sha = ? WHERE id = ?`, commit, id)
}

// FailStalePending marks pending revisions created before cutoff failed and
// returns how many moved: the row lands before its git commit, so a crash
// between the two leaves a pending row nothing else ever revisits.
func (s *DB) FailStalePending(cutoff int64) (int, error) {
	res, err := s.db.Exec(`UPDATE config_revisions SET status = 'failed'
		WHERE status = 'pending' AND created_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: fail stale revisions: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SetRevisionStatus moves one revision to applied or failed (the revert
// status is recorded as a new row by the caller).
func (s *DB) SetRevisionStatus(id, status string) error {
	return s.execOne("set revision status", `UPDATE config_revisions SET status = ? WHERE id = ?`, status, id)
}
