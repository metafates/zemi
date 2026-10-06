// memory.go is the frozen memory table (DESIGN §7): the pre-migration
// trail, read only - live rows imported into the brain at open (§8), the
// rows staying for /memory's merged list and the trail. A scope's table
// lives in its own DB: user-scope in the global DB, project-scope in the
// project DB.

package store

// Scope is where a memory holds: ScopeUser in every project, ScopeProject
// in one repository, each in its own DB (DESIGN §7).
type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

var Scopes = []Scope{ScopeUser, ScopeProject}

// Origin is who wrote a memory (DESIGN §7); the render labels and the
// supersede guard read it.
type Origin string

const (
	OriginUser    Origin = "user"
	OriginLearned Origin = "learned"
)

var Origins = []Origin{OriginUser, OriginLearned}

// Memory is one remembered fact. The table is frozen: no code writes it,
// and its rows keep their text, quote and provenance forever.
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

const memoryCols = `id, scope, text, quote, COALESCE(provenance_session_id, ''),
	created_at, updated_at, COALESCE(superseded_by, ''), origin`

func scanMemory(row scanner) (*Memory, error) {
	var m Memory
	if err := row.Scan(&m.ID, &m.Scope, &m.Text, &m.Quote, &m.ProvenanceSessionID,
		&m.CreatedAt, &m.UpdatedAt, &m.SupersededBy, &m.Origin); err != nil {
		return nil, err
	}
	return &m, nil
}

// GetMemory loads one memory by id, superseded or not.
func (s *DB) GetMemory(id string) (*Memory, error) {
	return one(s.db.QueryRow(`SELECT `+memoryCols+` FROM memory WHERE id = ?`, id), scanMemory)
}

// ListMemory returns a scope's memories, newest first; superseded rows are
// included only when asked.
func (s *DB) ListMemory(scope Scope, includeSuperseded bool) ([]*Memory, error) {
	rows, err := s.db.Query(`SELECT `+memoryCols+` FROM memory WHERE scope = ? AND (? OR superseded_by IS NULL)
		ORDER BY updated_at DESC`, scope, includeSuperseded)
	return collect(rows, err, scanMemory)
}
