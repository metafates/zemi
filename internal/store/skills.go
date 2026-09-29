// skills.go is the skills table (DESIGN §7, §10.5): the
// enabled-state store for discovered skills. Skills are user-scope: they
// live in the global DB. Discovery (internal/skill) drives which rows
// exist; SyncSkills reconciles the table with the filesystem.

package store

import (
	"database/sql"
	"fmt"
)

// Skill is one discovered skill's row; a new row starts enabled.
type Skill struct {
	Name      string
	Path      string
	Source    string // user | builtin | plugin
	Enabled   bool
	CreatedAt int64
}

func scanSkill(row scanner) (*Skill, error) {
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
	return one(s.db.QueryRow(`SELECT name, path, source, enabled, created_at FROM skills WHERE name = ?`, name), scanSkill)
}

// ListSkills returns all skill rows ordered by name.
func (s *DB) ListSkills() ([]*Skill, error) {
	rows, err := s.db.Query(`SELECT name, path, source, enabled, created_at FROM skills ORDER BY name`)
	return collect(rows, err, scanSkill)
}

// SetSkillEnabled flips the enabled flag of one skill row.
func (s *DB) SetSkillEnabled(name string, enabled bool) error {
	return s.execOne("set skill enabled", `UPDATE skills SET enabled = ? WHERE name = ?`, enabled, name)
}

// SyncSkills reconciles the skills table with filesystem discovery:
// discovered skills are inserted enabled or have their path and source
// refreshed, keeping the stored enabled choice; rows for skills no longer
// on disk are deleted.
func (s *DB) SyncSkills(discovered []*Skill) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("store: sync skills: %w", err)
		}
	}()
	return s.inTx(func(tx *sql.Tx) error {
		names := make([]string, len(discovered))
		for i, sk := range discovered {
			names[i] = sk.Name
			if _, err := tx.Exec(`INSERT INTO skills (name, path, source, enabled, created_at)
				VALUES (?, ?, ?, 1, ?)
				ON CONFLICT(name) DO UPDATE SET path = excluded.path, source = excluded.source`,
				sk.Name, sk.Path, sk.Source, now()); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`DELETE FROM skills WHERE name NOT IN (SELECT value FROM json_each(?))`, jsonList(names))
		return err
	})
}
