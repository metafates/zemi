// memory.go is the memory store (DESIGN §7): user-scope memory lives in
// the global DB, project-scope in the project DB. Every live row reaches
// the prompt, so there is no search; superseded rows stay for the trail.
package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Scope is where a memory holds: ScopeUser in every project, ScopeProject
// in one repository, each in its own DB (DESIGN §7).
type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

var Scopes = []Scope{ScopeUser, ScopeProject}

// Origin is who wrote a memory (DESIGN §7); the digest labels and the
// supersede guard read it.
type Origin string

const (
	OriginUser    Origin = "user"
	OriginLearned Origin = "learned"
)

var Origins = []Origin{OriginUser, OriginLearned}

// Memory is one remembered fact.
type Memory struct {
	ID                  string
	Scope               Scope
	Text                string // capped and scrubbed at intake (learn.MaxTextLen)
	Quote               string // the user's own words behind a memory about them (learn.MaxQuoteLen)
	ProvenanceSessionID string
	CreatedAt           int64
	UpdatedAt           int64
	SupersededBy        string
	Origin              Origin
}

const memoryCols = `id, scope, text, quote, provenance_session_id,
	created_at, updated_at, superseded_by, origin`

func scanMemory(row scanner) (*Memory, error) {
	var (
		m         Memory
		prov      any
		supersede any
	)
	if err := row.Scan(&m.ID, &m.Scope, &m.Text, &m.Quote, &prov,
		&m.CreatedAt, &m.UpdatedAt, &supersede, &m.Origin); err != nil {
		return nil, err
	}
	m.ProvenanceSessionID = asString(prov)
	m.SupersededBy = asString(supersede)
	return &m, nil
}

// AddMemory inserts one memory row; empty id and timestamps are filled, and
// a row copied from another scope keeps its age.
func (s *DB) AddMemory(m *Memory) error {
	defer s.memGen.Add(1)
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.Scope == "" || m.Text == "" || m.Origin == "" {
		return errors.New("store: memory needs scope, text and origin")
	}
	t := now()
	if m.CreatedAt == 0 {
		m.CreatedAt = t
	}
	if m.UpdatedAt == 0 {
		m.UpdatedAt = t
	}
	_, err := s.db.Exec(`INSERT INTO memory
		(id, scope, text, quote, provenance_session_id, created_at, updated_at, superseded_by, origin)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?)`,
		m.ID, m.Scope, m.Text, m.Quote, nullString(m.ProvenanceSessionID), m.CreatedAt, m.UpdatedAt, m.Origin)
	if err != nil {
		return fmt.Errorf("store: add memory: %w", err)
	}
	return nil
}

// GetMemory loads one memory by id, superseded or not.
func (s *DB) GetMemory(id string) (*Memory, error) {
	row := s.db.QueryRow(`SELECT `+memoryCols+` FROM memory WHERE id = ?`, id)
	m, err := scanMemory(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// ListMemory returns a scope's memories, newest first; superseded rows are
// included only when asked.
func (s *DB) ListMemory(scope Scope, includeSuperseded bool) ([]*Memory, error) {
	q := `SELECT ` + memoryCols + ` FROM memory WHERE scope = ?`
	if !includeSuperseded {
		q += ` AND superseded_by IS NULL`
	}
	q += ` ORDER BY updated_at DESC`
	rows, err := s.db.Query(q, scope)
	return collect(rows, err, scanMemory)
}

// UpdateMemoryText rewrites a memory's text and origin. Unknown ids are
// ErrNotFound.
func (s *DB) UpdateMemoryText(id, text string, origin Origin) error {
	defer s.memGen.Add(1)
	return s.execOne("update memory text", `UPDATE memory SET text = ?, origin = ?, updated_at = ?
		WHERE id = ?`, text, origin, now(), id)
}

// SupersedeMemory marks one memory as replaced by another; the row stays
// (the supersede guard reads the trail). Unknown ids are ErrNotFound.
func (s *DB) SupersedeMemory(id, byID string) error {
	defer s.memGen.Add(1)
	return s.execOne("supersede memory", `UPDATE memory SET superseded_by = ?, updated_at = ?
		WHERE id = ?`, nullString(byID), now(), id)
}

// DeleteMemory removes a memory row. Unknown ids are ErrNotFound.
func (s *DB) DeleteMemory(id string) error {
	defer s.memGen.Add(1)
	return s.execOne("delete memory", `DELETE FROM memory WHERE id = ?`, id)
}

// DeleteLearnedBetween deletes the learned memories a session wrote from
// from to until, both included, and returns their ids.
func (s *DB) DeleteLearnedBetween(sessionID string, from, until int64) ([]string, error) {
	defer s.memGen.Add(1)
	rows, err := s.db.Query(`DELETE FROM memory WHERE origin = 'learned'
		AND provenance_session_id = ? AND created_at BETWEEN ? AND ? RETURNING id`, sessionID, from, until)
	return collect(rows, err, func(r scanner) (id string, err error) {
		err = r.Scan(&id)
		return id, err
	})
}

// Unsupersede makes the memories ids replaced live again.
func (s *DB) Unsupersede(ids []string) error {
	defer s.memGen.Add(1)
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	_, err := s.db.Exec(`UPDATE memory SET superseded_by = NULL WHERE superseded_by IN (`+placeholders(len(ids))+`)`, args...)
	return err
}
