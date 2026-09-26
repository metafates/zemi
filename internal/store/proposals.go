package store

import (
	"database/sql"
	"errors"
)

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
		p.ID = NewID()
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
	return collect(rows, err, func(r scanner) (*Proposal, error) {
		var p Proposal
		return &p, r.Scan(&p.ID, &p.Payload, &p.CreatedAt)
	})
}

// TakeProposal deletes one proposal and returns it; ErrNotFound when absent.
func (s *DB) TakeProposal(id string) (*Proposal, error) {
	var p Proposal
	err := s.db.QueryRow(`DELETE FROM proposals WHERE id = ? RETURNING id, payload, created_at`, id).
		Scan(&p.ID, &p.Payload, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &p, err
}

// DeleteProposalsBetween drops the proposals a session made from from to
// until, both included.
func (s *DB) DeleteProposalsBetween(sessionID string, from, until int64) error {
	_, err := s.db.Exec(`DELETE FROM proposals WHERE created_at BETWEEN ? AND ?
		AND json_extract(payload, '$.ProvenanceSessionID') = ?`, from, until, sessionID)
	return err
}
