package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
	prompt_tokens, completion_tokens, cost`

func scanSession(row scanner) (*Session, error) {
	var (
		s      Session
		parent any
	)
	if err := row.Scan(&s.ID, &parent, &s.Title, &s.Profile,
		&s.CreatedAt, &s.UpdatedAt,
		&s.PromptTokens, &s.CompletionTokens, &s.Cost); err != nil {
		return nil, err
	}
	s.ParentID = asString(parent)
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

// FindSession loads the session whose id starts with prefix: a subagent
// run's id heads its child session's id.
func (s *DB) FindSession(prefix string) (*Session, error) {
	row := s.db.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE substr(id, 1, ?) = ? LIMIT 1`, len(prefix), prefix)
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

// ChildSessions returns the subagent runs a session started, newest first.
func (s *DB) ChildSessions(parentID string) ([]*Session, error) {
	rows, err := s.db.Query(`SELECT `+sessionCols+` FROM sessions WHERE parent_id = ? ORDER BY created_at DESC`, parentID)
	return collect(rows, err, scanSession)
}

// RenameSession sets the session title. Unknown ids are ErrNotFound.
func (s *DB) RenameSession(id, title string) error {
	return s.execOne("rename session", `UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?`,
		title, now(), id)
}

// DeleteSession removes a session and its subagent runs (parent_id has no
// foreign key; the rest cascades), then rewrites the file and empties the
// WAL: a DELETE only frees pages, so the text would stay on disk.
func (s *DB) DeleteSession(id string) error {
	if err := s.execOne("delete session", `DELETE FROM sessions WHERE id = ? OR parent_id = ?`, id, id); err != nil {
		return err
	}
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("store: vacuum: %w", err)
	}
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("store: checkpoint: %w", err)
	}
	return nil
}

// AddUsage accumulates token counts and cost onto a session. Unknown ids are
// ErrNotFound: silently dropping a turn's tokens and cost is worse than a
// caller seeing the miss.
func (s *DB) AddUsage(id string, prompt, completion int64, cost float64) error {
	return s.execOne("add usage", `UPDATE sessions SET
		prompt_tokens = prompt_tokens + ?,
		completion_tokens = completion_tokens + ?,
		cost = cost + ?
		WHERE id = ?`, prompt, completion, cost, id)
}

// Message is one persisted message: role plus fantasy content parts as JSON.
type Message struct {
	ID        string
	SessionID string
	Role      string
	PartsJSON string
	CreatedAt int64
}

// execer is what *sql.DB and *sql.Tx share, so one insert serves both.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertMessage(ex execer, m *Message) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = now()
	}
	_, err := ex.Exec(`INSERT INTO messages (id, session_id, role, parts_json, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		m.ID, m.SessionID, m.Role, m.PartsJSON, m.CreatedAt)
	if err == nil {
		// updated_at means new messages: a late bill for a closed session
		// must not make it the most recent one.
		_, err = ex.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, now(), m.SessionID)
	}
	if err != nil {
		return fmt.Errorf("store: add message: %w", err)
	}
	return nil
}

// AddMessage inserts a message row.
func (s *DB) AddMessage(m *Message) error { return insertMessage(s.db, m) }

// AddCompaction stores a compaction's summary row, the verbatim tail copies
// that follow it in one transaction: a crash partway would leave a resumed
// conversation missing the tail.
func (s *DB) AddCompaction(sessionID string, summary *Message, tail []*Message) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range append([]*Message{summary}, tail...) {
		m.SessionID = sessionID
		if err := insertMessage(tx, m); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ForkSession copies src into a new top-level session id: its messages
// under new ids, their checkpoints (a rewind in the fork restores files too),
// its tool calls and its subagents' (on the first message: the fork has no
// children, and the calls gate memory autonomy), in one transaction so a
// failure leaves no half-copied fork.
func (s *DB) ForkSession(src *Session, id, title string) error {
	msgs, err := s.Messages(src.ID)
	if err != nil {
		return err
	}
	type checkpoint struct {
		mid, path string
		existed   bool
		content   []byte
	}
	rows, err := s.db.Query(`SELECT message_id, path, existed, content FROM checkpoints WHERE session_id = ?`, src.ID)
	cps, err := collect(rows, err, func(r scanner) (c checkpoint, err error) {
		err = r.Scan(&c.mid, &c.path, &c.existed, &c.content)
		return
	})
	if err != nil {
		return fmt.Errorf("store: fork session: %w", err)
	}
	rows, err = s.db.Query(`SELECT t.message_id, t.tool, t.ok, t.created_at
		FROM tool_calls t JOIN messages m ON m.id = t.message_id
		WHERE m.session_id = ? OR m.session_id IN (SELECT id FROM sessions WHERE parent_id = ?)`, src.ID, src.ID)
	type call struct {
		ToolCall
		at int64
	}
	calls, err := collect(rows, err, func(r scanner) (c call, err error) {
		err = r.Scan(&c.MessageID, &c.Tool, &c.OK, &c.at)
		return
	})
	if err != nil {
		return fmt.Errorf("store: fork session: %w", err)
	}
	// every row is read before the transaction: one connection serves both
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	if _, err := tx.Exec(`INSERT INTO sessions (id, title, profile, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		id, title, src.Profile, t, t); err != nil {
		return fmt.Errorf("store: fork session: %w", err)
	}
	ids := make(map[string]string, len(msgs))
	for _, m := range msgs {
		copied := &Message{SessionID: id, Role: m.Role, PartsJSON: m.PartsJSON, CreatedAt: m.CreatedAt}
		if err := insertMessage(tx, copied); err != nil {
			return err
		}
		ids[m.ID] = copied.ID
	}
	for _, c := range cps {
		if mid, ok := ids[c.mid]; ok {
			if _, err := tx.Exec(`INSERT INTO checkpoints (message_id, session_id, path, existed, content, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
				mid, id, c.path, c.existed, c.content, t); err != nil {
				return fmt.Errorf("store: fork session: %w", err)
			}
		}
	}
	for _, c := range calls {
		mid, ok := ids[c.MessageID]
		if !ok && len(msgs) > 0 {
			mid = ids[msgs[0].ID]
		}
		if mid == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO tool_calls (message_id, tool, ok, created_at) VALUES (?, ?, ?, ?)`,
			mid, c.Tool, c.OK, c.at); err != nil {
			return fmt.Errorf("store: fork session: %w", err)
		}
	}
	return tx.Commit()
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

// MessageCount is how many rows Messages would return, tool telemetry
// included, without loading them.
func (s *DB) MessageCount(sessionID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&n)
	return n, err
}

// UserMessages returns the newest top-level session user messages, newest
// first, for the composer history: what the user typed survives the session
// it was typed in. Subagent (child) sessions are excluded, since their user
// rows are prompts the agent wrote, not the user.
func (s *DB) UserMessages(limit int) ([]*Message, error) {
	rows, err := s.db.Query(`SELECT id, session_id, role, parts_json, created_at
		FROM messages WHERE role = 'user'
		AND session_id IN (SELECT id FROM sessions WHERE COALESCE(parent_id, '') = '')
		ORDER BY rowid DESC LIMIT ?`, limit)
	return collect(rows, err, func(r scanner) (*Message, error) {
		var m Message
		return &m, r.Scan(&m.ID, &m.SessionID, &m.Role, &m.PartsJSON, &m.CreatedAt)
	})
}

// SessionPreview is what the session picker shows besides the title: how
// many messages a session holds and the parts of its first user message.
type SessionPreview struct {
	Count     int
	FirstJSON string
}

// SessionPreviews returns one preview per listed session; tool telemetry and
// compaction tails have their own roles and stay out of the count. The ids
// bound the scan: the picker shows a page of sessions, not every message
// ever stored.
func (s *DB) SessionPreviews(ids []string) (map[string]SessionPreview, error) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.Query(`SELECT session_id, COUNT(*), COALESCE((
			SELECT parts_json FROM messages f
			WHERE f.session_id = m.session_id AND f.role = 'user' ORDER BY rowid LIMIT 1), '')
		FROM messages m WHERE role IN ('user', 'assistant')
		AND session_id IN (`+placeholders(len(ids))+`) GROUP BY session_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SessionPreview{}
	for rows.Next() {
		var id string
		var p SessionPreview
		if err := rows.Scan(&id, &p.Count, &p.FirstJSON); err != nil {
			return nil, err
		}
		out[id] = p
	}
	return out, rows.Err()
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
	OK        bool
}

// AddToolCall records a tool invocation together with the message row it
// points at, in one transaction: a telemetry row that fails to land after
// its message leaves an orphan and the reflector's counts short.
func (s *DB) AddToolCall(m *Message, tc *ToolCall) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertMessage(tx, m); err != nil {
		return err
	}
	tc.MessageID = m.ID
	if _, err := tx.Exec(`INSERT INTO tool_calls (message_id, tool, ok, created_at) VALUES (?, ?, ?, ?)`,
		tc.MessageID, tc.Tool, tc.OK, now()); err != nil {
		return fmt.Errorf("store: add tool call: %w", err)
	}
	return tx.Commit()
}

// ToolStat is one per-tool aggregate over a session's tool calls.
type ToolStat struct {
	Tool   string
	Calls  int
	Failed int
}

// ToolCallStats aggregates a session's tool_calls rows per tool, busiest
// first (reflector input, DESIGN §10.3); afterID limits it to the calls
// stored after that message, "" counts them all.
func (s *DB) ToolCallStats(sessionID, afterID string) ([]ToolStat, error) {
	rows, err := s.db.Query(`SELECT t.tool, COUNT(*),
			SUM(CASE WHEN t.ok = 0 THEN 1 ELSE 0 END)
		FROM tool_calls t JOIN messages m ON m.id = t.message_id
		WHERE m.session_id = ? AND (? = '' OR m.rowid > (SELECT rowid FROM messages WHERE id = ?))
		GROUP BY t.tool ORDER BY COUNT(*) DESC, t.tool`, sessionID, afterID, afterID)
	return collect(rows, err, func(r scanner) (st ToolStat, err error) {
		err = r.Scan(&st.Tool, &st.Calls, &st.Failed)
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

// SetKVOnce inserts a json value by key only when absent, reporting
// whether this call inserted it. One INSERT OR IGNORE is atomic across
// processes (SQLite serializes writers), so it is the primitive a
// cross-process claim uses: two processes claiming the same key, one wins.
func (s *DB) SetKVOnce(key, valueJSON string) (bool, error) {
	res, err := s.db.Exec(`INSERT INTO kv (key, value_json, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO NOTHING`, key, valueJSON, now())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ReclaimKVIfStale overwrites key when its updated_at is older than
// olderThan, in one statement; it reports whether it did. The lease an
// owner that crashed leaves behind is reclaimed this way.
func (s *DB) ReclaimKVIfStale(key, valueJSON string, olderThan int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE kv SET value_json = ?, updated_at = ? WHERE key = ? AND updated_at < ?`,
		valueJSON, now(), key, olderThan)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DelKV drops a key, reporting whether there was one.
func (s *DB) DelKV(key string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM kv WHERE key = ?`, key)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// KeysKV lists the keys with a prefix, sorted.
func (s *DB) KeysKV(prefix string) ([]string, error) {
	rows, err := s.db.Query(`SELECT key FROM kv WHERE key LIKE ? ESCAPE '\' ORDER BY key`, likePrefix(prefix))
	return collect(rows, err, func(r scanner) (k string, err error) {
		err = r.Scan(&k)
		return
	})
}

// likePrefix escapes a LIKE prefix, so a key holding % or _ matches itself.
func likePrefix(prefix string) string {
	r := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	return r.Replace(prefix) + "%"
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
