package store

import (
	"database/sql"
	"errors"
)

// Proposal is a learned change parked for review (DESIGN §10.4). Payload
// is the kind's JSON (a Memory for kind "memory"); rows persist, so a
// proposal survives restarts until the user decides.
type Proposal struct {
	ID        string
	Kind      string
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
	_, err := s.db.Exec(`INSERT INTO proposals (id, kind, payload, created_at) VALUES (?, ?, ?, ?)`,
		p.ID, p.Kind, p.Payload, p.CreatedAt)
	return err
}

// Proposals lists one kind's pending proposals, oldest first.
func (s *DB) Proposals(kind string) ([]*Proposal, error) {
	rows, err := s.db.Query(`SELECT id, kind, payload, created_at FROM proposals WHERE kind = ? ORDER BY rowid`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Proposal
	for rows.Next() {
		var p Proposal
		if err := rows.Scan(&p.ID, &p.Kind, &p.Payload, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// TakeProposal deletes one proposal and returns it; ErrNotFound when absent.
func (s *DB) TakeProposal(id string) (*Proposal, error) {
	var p Proposal
	err := s.db.QueryRow(`DELETE FROM proposals WHERE id = ? RETURNING id, kind, payload, created_at`, id).
		Scan(&p.ID, &p.Kind, &p.Payload, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &p, err
}
