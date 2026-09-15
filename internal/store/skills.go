// skills.go implements the M3 skills table (DESIGN §7, §10.5): the
// enabled-state store for discovered skills. Skills are user-scope: they
// live in the global DB. Discovery (internal/skills) drives which rows
// exist; SyncSkills reconciles the table with the filesystem.
package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Skill is one discovered skill's row. DefaultEnabled is the enabled
// value used when the row is first inserted (authored skills start
// enabled, learned ones start disabled); it is not stored.
type Skill struct {
	Name           string
	Path           string
	Source         string // authored | learned
	Enabled        bool
	DefaultEnabled bool
	CreatedAt      int64
}

func (s *DB) scanSkill(row interface{ Scan(...any) error }) (*Skill, error) {
	var sk Skill
	var enabled int
	if err := row.Scan(&sk.Name, &sk.Path, &sk.Source, &enabled, &sk.CreatedAt); err != nil {
		return nil, err
	}
	sk.Enabled = enabled != 0
	return &sk, nil
}

// GetSkill returns one skill row by name.
func (s *DB) GetSkill(name string) (*Skill, error) {
	row := s.db.QueryRow(`SELECT name, path, source, enabled, created_at FROM skills WHERE name = ?`, name)
	sk, err := s.scanSkill(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sk, err
}

// ListSkills returns all skill rows ordered by name.
func (s *DB) ListSkills() ([]*Skill, error) {
	rows, err := s.db.Query(`SELECT name, path, source, enabled, created_at FROM skills ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list skills: %w", err)
	}
	defer rows.Close()
	var out []*Skill
	for rows.Next() {
		sk, err := s.scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// SetSkillEnabled flips the enabled flag of one skill row.
func (s *DB) SetSkillEnabled(name string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE skills SET enabled = ? WHERE name = ?`, v, name)
	if err != nil {
		return fmt.Errorf("store: set skill enabled: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// SyncSkills reconciles the skills table with filesystem discovery:
// discovered skills are inserted (with their default enabled state) or
// have their path/source refreshed, keeping the stored enabled choice;
// rows for skills no longer on disk are deleted.
func (s *DB) SyncSkills(discovered []*Skill) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: sync skills: %w", err)
	}
	defer tx.Rollback()
	keep := make(map[string]bool, len(discovered))
	for _, sk := range discovered {
		keep[sk.Name] = true
		def := 0
		if sk.DefaultEnabled {
			def = 1
		}
		if _, err := tx.Exec(`INSERT INTO skills (name, path, source, enabled, created_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET path = excluded.path, source = excluded.source`,
			sk.Name, sk.Path, sk.Source, def, now()); err != nil {
			return fmt.Errorf("store: sync skills: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM skills WHERE name NOT IN (`+placeholders(len(keep))+`)`,
		keys(keep)...); err != nil {
		return fmt.Errorf("store: sync skills: %w", err)
	}
	return tx.Commit()
}

func placeholders(n int) string {
	if n == 0 {
		return "''"
	}
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ","
		}
		out += "?"
	}
	return out
}

func keys(m map[string]bool) []any {
	out := make([]any, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
