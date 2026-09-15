// memory.go implements the M2 memory store (DESIGN §7): user-scope memory
// lives in the global DB, project-scope in the project DB. Search is FTS5
// over memory.text (external-content table kept in sync by triggers);
// superseded memories never rank.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Memory is one remembered fact.
type Memory struct {
	ID                 string
	Scope              string   // "user" or "project"
	Kind               string   // fact | gotcha | preference | convention | decision
	Text               string   // <= 512 chars, scrubbed at intake
	Tags               []string // comma-joined in the DB
	ProvenanceSessionID string
	Confidence         float64
	HitCount           int64
	CreatedAt          int64
	UpdatedAt          int64
	LastHitAt          int64
	SupersededBy       string
	Origin             string // "user" | "learned"
	// Score is the FTS5 match score (higher is better); 0 for
	// non-search listings.
	Score float64
}

const memoryCols = `id, scope, kind, text, tags, provenance_session_id,
	confidence, hit_count, created_at, updated_at, last_hit_at,
	superseded_by, origin`

// qualified with the memory alias for joins against memory_fts (both have
// a text column).
const memoryColsQualified = `m.id, m.scope, m.kind, m.text, m.tags,
	m.provenance_session_id, m.confidence, m.hit_count, m.created_at,
	m.updated_at, m.last_hit_at, m.superseded_by, m.origin`

func scanMemory(row interface{ Scan(...any) error }) (*Memory, error) {
	var (
		m         Memory
		prov      any
		supersede any
		tags      string
	)
	if err := row.Scan(&m.ID, &m.Scope, &m.Kind, &m.Text, &tags, &prov,
		&m.Confidence, &m.HitCount, &m.CreatedAt, &m.UpdatedAt, &m.LastHitAt,
		&supersede, &m.Origin, &m.Score); err != nil {
		return nil, err
	}
	m.ProvenanceSessionID = asString(prov)
	m.SupersededBy = asString(supersede)
	if tags != "" {
		m.Tags = strings.Split(tags, ",")
	}
	return &m, nil
}

// AddMemory inserts one memory row; empty id and timestamps are filled.
func (s *DB) AddMemory(m *Memory) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.Scope == "" || m.Kind == "" || m.Text == "" || m.Origin == "" {
		return errors.New("store: memory needs scope, kind, text and origin")
	}
	if m.Confidence == 0 {
		m.Confidence = 1
	}
	t := now()
	if m.CreatedAt == 0 {
		m.CreatedAt = t
	}
	m.UpdatedAt = t
	_, err := s.db.Exec(`INSERT INTO memory
		(id, scope, kind, text, tags, provenance_session_id, confidence,
		 hit_count, created_at, updated_at, last_hit_at, superseded_by, origin)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, 0, NULL, ?)`,
		m.ID, m.Scope, m.Kind, m.Text, strings.Join(m.Tags, ","),
		nullString(m.ProvenanceSessionID), m.Confidence,
		m.CreatedAt, m.UpdatedAt, m.Origin)
	if err != nil {
		return fmt.Errorf("store: add memory: %w", err)
	}
	return nil
}

// GetMemory loads one memory by id, superseded or not.
func (s *DB) GetMemory(id string) (*Memory, error) {
	row := s.db.QueryRow(`SELECT ` + memoryCols + `, 0 AS score FROM memory WHERE id = ?`, id)
	m, err := scanMemory(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// SearchMemory full-text searches one scope's live memories and returns
// them best-first (score = -bm25 rank). An empty query falls back to
// recency order with score 0, which is what digest assembly wants when
// the user prompt carries no keywords. With bump=true every hit has its
// hit_count incremented and last_hit_at stamped (DESIGN §7).
func (s *DB) SearchMemory(scope, query string, limit int, bump bool) ([]*Memory, error) {
	if strings.TrimSpace(query) == "" {
		rows, err := s.db.Query(`SELECT `+memoryCols+`, 0 AS score
			FROM memory WHERE scope = ? AND superseded_by IS NULL
			ORDER BY updated_at DESC LIMIT ?`, scope, limit)
		if err != nil {
			return nil, err
		}
		return scanMemories(rows)
	}

	rows, err := s.db.Query(`SELECT `+memoryColsQualified+`, -f.rank AS score
		FROM memory_fts f JOIN memory m ON m.rowid = f.rowid
		WHERE memory_fts MATCH ? AND m.scope = ? AND m.superseded_by IS NULL
		ORDER BY f.rank LIMIT ?`, ftsQuery(query), scope, limit)
	if err != nil {
		return nil, err
	}
	out, err := scanMemories(rows)
	if err != nil || len(out) == 0 || !bump {
		return out, err
	}
	ids := make([]string, len(out))
	for i, m := range out {
		ids[i] = m.ID
	}
	ph := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, now())
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	if _, err := s.db.Exec(`UPDATE memory SET hit_count = hit_count + 1,
		last_hit_at = ? WHERE id IN (`+strings.Join(ph, ",")+`)`, args...); err != nil {
		return out, fmt.Errorf("store: memory hit bump: %w", err)
	}
	return out, nil
}

func scanMemories(rows *sql.Rows) ([]*Memory, error) {
	defer rows.Close()
	var out []*Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ftsQuery turns free text into an OR of quoted FTS5 tokens, so arbitrary
// user input never becomes fts5 syntax.
func ftsQuery(query string) string {
	tokens := strings.FieldsFunc(query, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r > 0x7f)
	})
	if len(tokens) == 0 {
		return `""`
	}
	for i, tok := range tokens {
		tokens[i] = `"` + strings.ReplaceAll(tok, `"`, `""`) + `"`
	}
	return strings.Join(tokens, " OR ")
}

// ListMemory returns a scope's memories, newest first; superseded rows are
// included only when asked.
func (s *DB) ListMemory(scope string, includeSuperseded bool) ([]*Memory, error) {
	q := `SELECT ` + memoryCols + `, 0 AS score FROM memory WHERE scope = ?`
	if !includeSuperseded {
		q += ` AND superseded_by IS NULL`
	}
	q += ` ORDER BY updated_at DESC`
	rows, err := s.db.Query(q, scope)
	if err != nil {
		return nil, err
	}
	return scanMemories(rows)
}

// UpdateMemoryText rewrites a memory's text; the update trigger re-syncs
// the FTS index. Unknown ids are ErrNotFound.
func (s *DB) UpdateMemoryText(id, text string) error {
	res, err := s.db.Exec(`UPDATE memory SET text = ?, updated_at = ?
		WHERE id = ?`, text, now(), id)
	if err != nil {
		return fmt.Errorf("store: update memory text: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SupersedeMemory marks one memory as replaced by another; the row stays
// (the supersede guard reads the trail). Unknown ids are ErrNotFound.
func (s *DB) SupersedeMemory(id, byID string) error {
	res, err := s.db.Exec(`UPDATE memory SET superseded_by = ?, updated_at = ?
		WHERE id = ?`, nullString(byID), now(), id)
	if err != nil {
		return fmt.Errorf("store: supersede memory: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteMemory removes a memory row; the FTS trigger keeps the index in
// sync. Unknown ids are ErrNotFound.
func (s *DB) DeleteMemory(id string) error {
	res, err := s.db.Exec(`DELETE FROM memory WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete memory: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
