package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotFound is returned by single getters when the row is missing.
var ErrNotFound = errors.New("store: not found")

// Session is one persisted conversation.
type Session struct {
	ID               string
	ParentID         string
	Title            string
	Profile          string
	CreatedAt        int64
	UpdatedAt        int64
	SummaryMessageID string
	PromptTokens     int64
	CompletionTokens int64
	Cost             float64
}

// CreateSession inserts a new session row.
func (s *DB) CreateSession(id, parentID, title, profile string) error {
	t := now()
	_, err := s.db.Exec(`INSERT INTO sessions
		(id, parent_id, title, profile, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, nullString(parentID), title, profile, t, t)
	if err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

const sessionCols = `id, parent_id, title, profile, created_at, updated_at,
	summary_message_id, prompt_tokens, completion_tokens, cost`

func scanSession(row scanner) (*Session, error) {
	var (
		s          Session
		parent     any
		summaryMsg any
	)
	if err := row.Scan(&s.ID, &parent, &s.Title, &s.Profile,
		&s.CreatedAt, &s.UpdatedAt, &summaryMsg,
		&s.PromptTokens, &s.CompletionTokens, &s.Cost); err != nil {
		return nil, err
	}
	s.ParentID = asString(parent)
	s.SummaryMessageID = asString(summaryMsg)
	return &s, nil
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// GetSession loads one session by id.
func (s *DB) GetSession(id string) (*Session, error) {
	row := s.db.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sess, err
}

// ListSessions returns top-level sessions (subagent runs excluded) ordered
// by most recent activity.
func (s *DB) ListSessions(limit int) ([]*Session, error) {
	rows, err := s.db.Query(`SELECT `+sessionCols+` FROM sessions WHERE COALESCE(parent_id, '') = '' ORDER BY updated_at DESC LIMIT ?`, limit)
	return collect(rows, err, scanSession)
}

// RenameSession sets the session title.
func (s *DB) RenameSession(id, title string) error {
	_, err := s.db.Exec(`UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?`,
		title, now(), id)
	return err
}

// SetSummary points a session at the message its history resumes from.
func (s *DB) SetSummary(id, messageID string) error {
	_, err := s.db.Exec(`UPDATE sessions SET summary_message_id = ?, updated_at = ? WHERE id = ?`,
		messageID, now(), id)
	return err
}

// AddUsage accumulates token counts and cost onto a session.
func (s *DB) AddUsage(id string, prompt, completion int64, cost float64) error {
	_, err := s.db.Exec(`UPDATE sessions SET
		prompt_tokens = prompt_tokens + ?,
		completion_tokens = completion_tokens + ?,
		cost = cost + ?,
		updated_at = ?
		WHERE id = ?`, prompt, completion, cost, now(), id)
	return err
}

// Message is one persisted message: role plus fantasy content parts as JSON.
type Message struct {
	ID        string
	SessionID string
	Role      string
	PartsJSON string
	CreatedAt int64
}

// AddMessage inserts a message row.
func (s *DB) AddMessage(m *Message) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = now()
	}
	_, err := s.db.Exec(`INSERT INTO messages (id, session_id, role, parts_json, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		m.ID, m.SessionID, m.Role, m.PartsJSON, m.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: add message: %w", err)
	}
	return nil
}

// Messages returns a session's messages in insertion order: rowid, since a
// turn persists several messages within one millisecond and ids are random.
func (s *DB) Messages(sessionID string) ([]*Message, error) {
	rows, err := s.db.Query(`SELECT id, session_id, role, parts_json, created_at
		FROM messages WHERE session_id = ? ORDER BY rowid`, sessionID)
	return collect(rows, err, func(r scanner) (*Message, error) {
		var m Message
		return &m, r.Scan(&m.ID, &m.SessionID, &m.Role, &m.PartsJSON, &m.CreatedAt)
	})
}

// Todo is one item of a session's todo list.
type Todo struct {
	Content    string
	Status     string
	ActiveForm string
	Ord        int
}

// SetTodos replaces a session's todo list.
func (s *DB) SetTodos(sessionID string, todos []Todo) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM todos WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	t := now()
	for i, td := range todos {
		if _, err := tx.Exec(`INSERT INTO todos
			(session_id, content, status, active_form, ord, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			sessionID, td.Content, td.Status, td.ActiveForm, i, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Todos returns a session's todo list in order.
func (s *DB) Todos(sessionID string) ([]Todo, error) {
	rows, err := s.db.Query(`SELECT content, status, active_form, ord
		FROM todos WHERE session_id = ? ORDER BY ord`, sessionID)
	return collect(rows, err, func(r scanner) (td Todo, err error) {
		err = r.Scan(&td.Content, &td.Status, &td.ActiveForm, &td.Ord)
		return
	})
}

// ToolCall is one telemetry row for a tool invocation.
type ToolCall struct {
	MessageID string
	Tool      string
	InputJSON string
	OK        bool
	ErrorKind string
	LatencyMS int64
	Denied    bool
	DecidedBy string
}

// AddToolCall records a tool invocation.
func (s *DB) AddToolCall(tc *ToolCall) error {
	_, err := s.db.Exec(`INSERT INTO tool_calls
		(message_id, tool, input_json, ok, error_kind, latency_ms, denied, decided_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tc.MessageID, tc.Tool, tc.InputJSON, tc.OK, tc.ErrorKind, tc.LatencyMS,
		tc.Denied, tc.DecidedBy, now())
	return err
}

// ToolStat is one per-tool aggregate over a session's tool calls.
type ToolStat struct {
	Tool   string
	Calls  int
	Failed int
	Denied int
}

// ToolCallStats aggregates a session's tool_calls rows per tool, busiest
// first (reflector input, DESIGN §10.3).
func (s *DB) ToolCallStats(sessionID string) ([]ToolStat, error) {
	rows, err := s.db.Query(`SELECT t.tool, COUNT(*),
			SUM(CASE WHEN t.ok = 0 THEN 1 ELSE 0 END),
			SUM(CASE WHEN t.denied != 0 THEN 1 ELSE 0 END)
		FROM tool_calls t JOIN messages m ON m.id = t.message_id
		WHERE m.session_id = ?
		GROUP BY t.tool ORDER BY COUNT(*) DESC, t.tool`, sessionID)
	return collect(rows, err, func(r scanner) (st ToolStat, err error) {
		err = r.Scan(&st.Tool, &st.Calls, &st.Failed, &st.Denied)
		return
	})
}

// GetKV loads a json value by key; ErrNotFound when absent.
func (s *DB) GetKV(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value_json FROM kv WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetKV writes a json value by key.
func (s *DB) SetKV(key, valueJSON string) error {
	_, err := s.db.Exec(`INSERT INTO kv (key, value_json, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at`,
		key, valueJSON, now())
	return err
}

// Usage is summed token counts and cost.
type Usage struct {
	Sessions           int
	Prompt, Completion int64
	Cost               float64
}

// UsageSince sums top-level sessions active at or after the unix-ms time
// (subagent runs already count toward their parent).
func (s *DB) UsageSince(ms int64) (Usage, error) {
	var u Usage
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0), COALESCE(SUM(cost), 0)
		FROM sessions WHERE COALESCE(parent_id, '') = '' AND updated_at >= ?`, ms).Scan(&u.Sessions, &u.Prompt, &u.Completion, &u.Cost)
	return u, err
}
