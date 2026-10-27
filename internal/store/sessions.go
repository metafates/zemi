package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrNotFound is returned by single getters when the row is missing.
var ErrNotFound = errors.New("store: not found")

// ErrSessionGone is returned by the writes that foreign-key to sessions when
// the session row is missing: a store deleted under this window, which
// repeats on every send.
var ErrSessionGone = errors.New("session was deleted")

// Session is one persisted conversation.
type Session struct {
	ID       string
	ParentID string
	Title    string
	// Agent is the agent a session runs as: "" for a main conversation, a
	// subagent run's definition name otherwise
	Agent            string
	CreatedAt        int64
	UpdatedAt        int64
	PromptTokens     int64
	CompletionTokens int64
	Cost             float64
	// cache tokens of the input above: what the provider read from its prompt
	// cache, over the input billed alongside them
	CacheRead   int64
	CacheBilled int64
	// LastIn is the context the latest step used, as the provider counted it
	LastIn int64
	// RunStatus is a subagent run's outcome, written where it ends: the run
	// rows and a restart read it back instead of assuming the run finished
	RunStatus string
}

func (s *DB) CreateSession(id, parentID, title, agent string) error {
	t := now()
	_, err := s.db.Exec(`INSERT INTO sessions
		(id, parent_id, title, agent, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, nullString(parentID), title, agent, t, t)
	if err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

const sessionCols = `id, COALESCE(parent_id, ''), title, agent, created_at, updated_at,
	prompt_tokens, completion_tokens, cost, cache_read_tokens, cache_billed_tokens, last_in, run_status`

func scanSession(row scanner) (*Session, error) {
	var s Session
	if err := row.Scan(&s.ID, &s.ParentID, &s.Title, &s.Agent,
		&s.CreatedAt, &s.UpdatedAt,
		&s.PromptTokens, &s.CompletionTokens, &s.Cost,
		&s.CacheRead, &s.CacheBilled, &s.LastIn, &s.RunStatus); err != nil {
		return nil, err
	}
	return &s, nil
}

// SetSessionRunStatus records how a run ended. It leaves updated_at alone: a
// run ending is not activity in its parent's list.
func (s *DB) SetSessionRunStatus(id, status string) error {
	return s.execOne("set run status", `UPDATE sessions SET run_status = ? WHERE id = ?`, status, id)
}

func (s *DB) GetSession(id string) (*Session, error) {
	return one(s.db.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id), scanSession)
}

// topLevelSessions is the WHERE the run lists read through: the anchor row
// the migration plants is not a conversation.
const topLevelSessions = `COALESCE(parent_id, '') = '' AND id <> '` + MainSessionID + `'`

// ListSessions returns top-level sessions (subagent runs excluded) ordered
// by most recent activity.
func (s *DB) ListSessions(limit int) ([]*Session, error) {
	rows, err := s.db.Query(`SELECT `+sessionCols+` FROM sessions WHERE `+topLevelSessions+` ORDER BY updated_at DESC LIMIT ?`, limit)
	return collect(rows, err, scanSession)
}

// ChildSessions returns the subagent runs a session started, newest first.
func (s *DB) ChildSessions(parentID string) ([]*Session, error) {
	rows, err := s.db.Query(`SELECT `+sessionCols+` FROM sessions WHERE parent_id = ? ORDER BY created_at DESC`, parentID)
	return collect(rows, err, scanSession)
}

// AddUsage accumulates token counts and cost onto a session. Unknown ids are
// ErrNotFound: silently dropping a turn's tokens and cost is worse than a
// caller seeing the miss.
func (s *DB) AddUsage(id string, prompt, completion, cacheRead, cacheBilled int64, cost float64) error {
	return s.execOne("add usage", `UPDATE sessions SET
		prompt_tokens = prompt_tokens + ?,
		completion_tokens = completion_tokens + ?,
		cache_read_tokens = cache_read_tokens + ?,
		cache_billed_tokens = cache_billed_tokens + ?,
		cost = cost + ?
		WHERE id = ?`, prompt, completion, cacheRead, cacheBilled, cost, id)
}

// Message is one persisted message: role plus fantasy content parts as JSON,
// and RowMeta, the rows the message draws that its text cannot say (a call's
// status, the run it started), as JSON.
type Message struct {
	ID        string
	SessionID string
	Role      string
	PartsJSON string
	RowMeta   string
	CreatedAt int64
}

// execer is what *sql.DB and *sql.Tx share, so one insert serves both.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// sessionGone reports an insert that failed its session foreign key: the
// tables keyed to sessions reference that one column, so the code is the
// proof the parent row is gone.
func sessionGone(err error) bool {
	e, ok := errors.AsType[*sqlite.Error](err)
	return ok && e.Code() == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY
}

func insertMessage(ex execer, m *Message) error {
	if m.ID == "" {
		m.ID = newID()
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = now()
	}
	_, err := ex.Exec(`INSERT INTO messages (id, session_id, role, parts_json, row_meta, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		m.ID, m.SessionID, m.Role, m.PartsJSON, m.RowMeta, m.CreatedAt)
	if err == nil {
		// updated_at means new messages: a late bill for a closed session
		// must not make it the most recent one.
		_, err = ex.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, now(), m.SessionID)
	}
	if err != nil {
		if sessionGone(err) {
			return fmt.Errorf("store: add message: %w", ErrSessionGone)
		}
		return fmt.Errorf("store: add message: %w", err)
	}
	return nil
}

func (s *DB) AddMessage(m *Message) error { return insertMessage(s.db, m) }

// Messages returns a session's messages in insertion order: rowid, since a
// turn persists several messages within one millisecond and ids are random.
func (s *DB) Messages(sessionID string) ([]*Message, error) {
	rows, err := s.db.Query(`SELECT id, session_id, role, parts_json, row_meta, created_at
		FROM messages WHERE session_id = ? ORDER BY rowid`, sessionID)
	return collect(rows, err, scanMessage)
}

func scanMessage(r scanner) (*Message, error) {
	var m Message
	return &m, r.Scan(&m.ID, &m.SessionID, &m.Role, &m.PartsJSON, &m.RowMeta, &m.CreatedAt)
}

// TodoStatus is where a todo item stands; the todos tool's schema offers
// TodoStatuses.
type TodoStatus string

const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoDone       TodoStatus = "done"
)

var TodoStatuses = []TodoStatus{TodoPending, TodoInProgress, TodoDone}

// Todo is one item of a session's todo list.
type Todo struct {
	Content    string
	Status     TodoStatus
	ActiveForm string
}

// SetTodos replaces a session's todo list.
func (s *DB) SetTodos(sessionID string, todos []Todo) error {
	return s.inTx(func(tx *sql.Tx) error { return setTodos(tx, sessionID, todos) })
}

func setTodos(tx *sql.Tx, sessionID string, todos []Todo) error {
	if _, err := tx.Exec(`DELETE FROM todos WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	t := now()
	for i, td := range todos {
		if _, err := tx.Exec(`INSERT INTO todos
			(session_id, content, status, active_form, ord, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			sessionID, td.Content, td.Status, td.ActiveForm, i, t); err != nil {
			if sessionGone(err) {
				return fmt.Errorf("store: set todos: %w", ErrSessionGone)
			}
			return err
		}
	}
	return nil
}

// Todos returns a session's todo list in order.
func (s *DB) Todos(sessionID string) ([]Todo, error) {
	rows, err := s.db.Query(`SELECT content, status, active_form
		FROM todos WHERE session_id = ? ORDER BY ord`, sessionID)
	return collect(rows, err, func(r scanner) (td Todo, err error) {
		err = r.Scan(&td.Content, &td.Status, &td.ActiveForm)
		return td, err
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
// its message leaves an orphan behind.
func (s *DB) AddToolCall(m *Message, tc *ToolCall) error {
	return s.inTx(func(tx *sql.Tx) error {
		if err := insertMessage(tx, m); err != nil {
			return err
		}
		tc.MessageID = m.ID
		if _, err := tx.Exec(`INSERT INTO tool_calls (message_id, tool, ok, created_at) VALUES (?, ?, ?, ?)`,
			tc.MessageID, tc.Tool, tc.OK, now()); err != nil {
			return fmt.Errorf("store: add tool call: %w", err)
		}
		return nil
	})
}

// ToolStat is one per-tool aggregate over a session's tool calls.
type ToolStat struct {
	Tool   string
	Calls  int
	Failed int
}

// ToolCallStats aggregates a session's tool_calls rows per tool, busiest
// first; afterID limits it to the calls stored after that message, ""
// counts them all.
func (s *DB) ToolCallStats(sessionID, afterID string) ([]ToolStat, error) {
	rows, err := s.db.Query(`SELECT t.tool, COUNT(*),
			SUM(CASE WHEN t.ok = 0 THEN 1 ELSE 0 END)
		FROM tool_calls t JOIN messages m ON m.id = t.message_id
		WHERE m.session_id = ? AND (? = '' OR m.rowid > (SELECT rowid FROM messages WHERE id = ?))
		GROUP BY t.tool ORDER BY COUNT(*) DESC, t.tool`, sessionID, afterID, afterID)
	return collect(rows, err, func(r scanner) (st ToolStat, err error) {
		err = r.Scan(&st.Tool, &st.Calls, &st.Failed)
		return st, err
	})
}

// GetKV loads a json value by key; ErrNotFound when absent.
func (s *DB) GetKV(key string) (string, error) {
	return one(s.db.QueryRow(`SELECT value_json FROM kv WHERE key = ?`, key), scanString)
}

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
	return s.changed(`INSERT INTO kv (key, value_json, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO NOTHING`, key, valueJSON, now())
}

// ReclaimKVIfStale overwrites key when its updated_at is older than
// olderThan, in one statement; it reports whether it did. The lease an
// owner that crashed leaves behind is reclaimed this way.
func (s *DB) ReclaimKVIfStale(key, valueJSON string, olderThan int64) (bool, error) {
	return s.changed(`UPDATE kv SET value_json = ?, updated_at = ? WHERE key = ? AND updated_at < ?`,
		valueJSON, now(), key, olderThan)
}

// DelKV drops a key, reporting whether there was one.
func (s *DB) DelKV(key string) (bool, error) {
	return s.changed(`DELETE FROM kv WHERE key = ?`, key)
}

// KeysKV lists the keys with a prefix, sorted.
func (s *DB) KeysKV(prefix string) ([]string, error) {
	rows, err := s.db.Query(`SELECT key FROM kv WHERE key LIKE ? ESCAPE '\' ORDER BY key`, likePrefix(prefix))
	return collect(rows, err, scanString)
}

// likePrefix escapes a LIKE prefix, so a key holding % or _ matches itself.
func likePrefix(prefix string) string {
	r := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	return r.Replace(prefix) + "%"
}

// Usage is summed token counts and cost.
type Usage struct {
	Prompt, Completion int64
	Cost               float64
}

// AddUsageDay accrues one bill's tokens and cost onto its UTC day, the dated
// sums /usage's buckets read. The anchor row holds the project's total; this
// holds when it was spent.
func (s *DB) AddUsageDay(prompt, completion int64, cost float64) error {
	day := time.Now().UTC().Format(time.DateOnly)
	_, err := s.db.Exec(`INSERT INTO usage_days (day, prompt_tokens, completion_tokens, cost)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(day) DO UPDATE SET
			prompt_tokens = prompt_tokens + excluded.prompt_tokens,
			completion_tokens = completion_tokens + excluded.completion_tokens,
			cost = cost + excluded.cost`,
		day, prompt, completion, cost)
	if err != nil {
		return fmt.Errorf("store: add usage day: %w", err)
	}
	return nil
}

// UsageSince sums the dated usage rows at or after the unix-ms time: the
// anchor row's accounting by day, subagent runs included through it (a run's
// bill rides the anchor with its parent's).
func (s *DB) UsageSince(ms int64) (Usage, error) {
	var u Usage
	day := ""
	if ms > 0 {
		day = time.UnixMilli(ms).UTC().Format(time.DateOnly)
	}
	err := s.db.QueryRow(`SELECT COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0), COALESCE(SUM(cost), 0)
		FROM usage_days WHERE ? = '' OR day >= ?`, day, day).Scan(&u.Prompt, &u.Completion, &u.Cost)
	return u, err
}
