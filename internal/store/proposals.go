package store

// Proposal is a learned memory parked for review (DESIGN §10.4). Payload
// is its JSON; rows persist, so a proposal survives restarts until the user
// decides.
type Proposal struct {
	ID        string
	Payload   string
	CreatedAt int64
}

func (s *DB) AddProposal(p *Proposal) error {
	if p.ID == "" {
		p.ID = ShortID()
	}
	if p.CreatedAt == 0 {
		p.CreatedAt = now()
	}
	_, err := s.db.Exec(`INSERT INTO proposals (id, payload, created_at) VALUES (?, ?, ?)`,
		p.ID, p.Payload, p.CreatedAt)
	return err
}

// Proposals lists the pending proposals, oldest first.
func (s *DB) Proposals() ([]*Proposal, error) {
	rows, err := s.db.Query(`SELECT id, payload, created_at FROM proposals ORDER BY rowid`)
	return collect(rows, err, scanProposal)
}

func scanProposal(r scanner) (*Proposal, error) {
	var p Proposal
	if err := r.Scan(&p.ID, &p.Payload, &p.CreatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// TakeProposal deletes one proposal and returns it; ErrNotFound when absent.
func (s *DB) TakeProposal(id string) (*Proposal, error) {
	return one(s.db.QueryRow(`DELETE FROM proposals WHERE id = ? RETURNING id, payload, created_at`, id), scanProposal)
}

// DeleteProposalsBetween drops the proposals a session made from from to
// until, both included.
func (s *DB) DeleteProposalsBetween(sessionID string, from, until int64) error {
	_, err := s.db.Exec(`DELETE FROM proposals WHERE created_at BETWEEN ? AND ?
		AND json_extract(payload, '$.ProvenanceSessionID') = ?`, from, until, sessionID)
	return err
}
