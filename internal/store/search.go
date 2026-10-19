// search.go finds a conversation by what was said in it (DESIGN §8). The
// matching runs in SQLite over every store zemi may open read-only, so it is
// one query per project and no walk of the rows in Go.

package store

import (
	"database/sql/driver"
	"fmt"
	"strings"

	"modernc.org/sqlite"
)

// zemi_has is substring matching that folds case beyond ASCII, which SQLite's
// LIKE does not: a Cyrillic question has to find its answer. Registered on the
// driver, so every connection, the read-only ones included, has it.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("zemi_has", 2, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		a, _ := args[0].(string)
		b, _ := args[1].(string)
		return strings.Contains(strings.ToLower(a), strings.ToLower(b)), nil
	})
}

// MessageHit is one text part a search matched.
type MessageHit struct {
	SessionID, Title, Role, Text string
	UpdatedAt, At                int64
	Count                        int
}

// SearchMessages returns the newest matches of every word, at most perSession
// snippets per session, all hits counted; top-level sessions only.
func (s *DB) SearchMessages(words []string, perSession, maxRows int) ([]MessageHit, error) {
	if len(words) == 0 {
		return nil, nil
	}
	conds := make([]string, len(words))
	args := make([]any, 0, len(words)+2)
	for i, w := range words {
		conds[i] = `zemi_has(json_extract(p.value, '$.data.text'), ?)`
		args = append(args, w)
	}
	args = append(args, perSession, maxRows)
	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT id, title, updated_at, role, at, text, total FROM (
			SELECT s.id AS id, s.title AS title, s.updated_at AS updated_at, m.role AS role,
				m.created_at AS at, json_extract(p.value, '$.data.text') AS text,
				COUNT(*) OVER (PARTITION BY s.id) AS total,
				ROW_NUMBER() OVER (PARTITION BY s.id ORDER BY m.rowid DESC) AS rn
			FROM messages m
			JOIN sessions s ON s.id = m.session_id
			JOIN json_each(m.parts_json, '$.content') p
			WHERE COALESCE(s.parent_id, '') = '' AND m.role IN ('user', 'assistant')
				AND json_extract(p.value, '$.type') = 'text' AND %s
		) WHERE rn <= ? ORDER BY total DESC, updated_at DESC, id, rn LIMIT ?`,
		strings.Join(conds, " AND ")), args...)
	return collect(rows, err, func(r scanner) (MessageHit, error) {
		var h MessageHit
		err := r.Scan(&h.SessionID, &h.Title, &h.UpdatedAt, &h.Role, &h.At, &h.Text, &h.Count)
		return h, err
	})
}
