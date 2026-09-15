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
// applied, failed or reverted; EvalOK is nil when evals did not run (they
// gate auto-apply from M4 on); Commit is the config-repo commit SHA once
// the git commit landed.
type ConfigRevision struct {
	ID          string
	CreatedAt   int64
	Title       string
	ChangesJSON string
	PathsJSON   string
	Status      string
	AppliedBy   string
	EvalOK      *bool
	Commit      string
}

const revisionCols = `id, created_at, title, changes_json, paths_json, status, applied_by, eval_ok, commit_sha`

func scanRevision(row interface{ Scan(...any) error }) (*ConfigRevision, error) {
	var r ConfigRevision
	var evalOK sql.NullInt64
	if err := row.Scan(&r.ID, &r.CreatedAt, &r.Title, &r.ChangesJSON,
		&r.PathsJSON, &r.Status, &r.AppliedBy, &evalOK, &r.Commit); err != nil {
		return nil, err
	}
	if evalOK.Valid {
		v := evalOK.Int64 != 0
		r.EvalOK = &v
	}
	return &r, nil
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
	var evalOK any
	if r.EvalOK != nil {
		evalOK = *r.EvalOK
	}
	_, err := s.db.Exec(`INSERT INTO config_revisions
		(id, created_at, title, changes_json, paths_json, status, applied_by, eval_ok, commit_sha)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.CreatedAt, r.Title, r.ChangesJSON, r.PathsJSON, r.Status, r.AppliedBy, evalOK, r.Commit)
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
	if err != nil {
		return nil, fmt.Errorf("store: list revisions: %w", err)
	}
	defer rows.Close()
	var out []*ConfigRevision
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRevisionCommit records the config-repo commit SHA of an applied
// revision (what :revision revert reverts).
func (s *DB) SetRevisionCommit(id, commit string) error {
	res, err := s.db.Exec(`UPDATE config_revisions SET commit_sha = ? WHERE id = ?`, commit, id)
	if err != nil {
		return fmt.Errorf("store: set revision commit: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetRevisionStatus moves one revision to applied or failed (the revert
// status is recorded as a new row by the caller).
func (s *DB) SetRevisionStatus(id, status string) error {
	res, err := s.db.Exec(`UPDATE config_revisions SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("store: set revision status: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}
