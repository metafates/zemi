-- +goose Up
CREATE TABLE IF NOT EXISTS sessions (
	id TEXT PRIMARY KEY,
	parent_id TEXT,
	title TEXT NOT NULL DEFAULT '',
	-- the column is really "which agent ran": it stores subagent run names too
	agent TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	cache_billed_tokens INTEGER NOT NULL DEFAULT 0,
	cost REAL NOT NULL DEFAULT 0,
	-- the context the latest step used: only the provider counts it, so a
	-- resume reads it back instead of estimating
	last_in INTEGER NOT NULL DEFAULT 0,
	run_status TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS sessions_parent ON sessions(parent_id);
-- the picker and the splash order by recency on every render
CREATE INDEX IF NOT EXISTS sessions_updated ON sessions(updated_at);

CREATE TABLE IF NOT EXISTS messages (
	id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	role TEXT NOT NULL,
	parts_json TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	-- a message's own rows: the facts its text cannot say, so a resumed run
	-- rebuilds the same rows (a call's status, its job or run)
	row_meta TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS messages_session ON messages(session_id);

CREATE TABLE IF NOT EXISTS todos (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	content TEXT NOT NULL,
	status TEXT NOT NULL,
	active_form TEXT NOT NULL DEFAULT '',
	ord INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS todos_session ON todos(session_id);

CREATE TABLE IF NOT EXISTS tool_calls (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	tool TEXT NOT NULL,
	ok INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS tool_calls_message ON tool_calls(message_id, created_at);

CREATE TABLE IF NOT EXISTS skills (
	name TEXT PRIMARY KEY,
	path TEXT NOT NULL,
	source TEXT NOT NULL,
	enabled INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS kv (
	key TEXT PRIMARY KEY,
	value_json TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);

-- The eternal chat: one append-only log per project DB, the binary tree of
-- one-line summaries built over it, and the persisted view. seq is the
-- 0-based message index, assigned max+1 inside the append transaction, never
-- AUTOINCREMENT: the index stays dense.
CREATE TABLE IF NOT EXISTS log (
	seq INTEGER PRIMARY KEY,
	kind TEXT NOT NULL,
	text TEXT NOT NULL,
	meta_json TEXT,
	parts_json TEXT,
	created_at TEXT NOT NULL
);

-- node(l, i) summarizes the 2^l messages from i*2^l on. INSERT OR IGNORE
-- dedupes concurrent builders; no session id, one main log per DB.
CREATE TABLE IF NOT EXISTS nodes (
	l INTEGER NOT NULL,
	i INTEGER NOT NULL,
	text TEXT NOT NULL,
	size INTEGER NOT NULL,
	built_at TEXT NOT NULL,
	PRIMARY KEY (l, i)
);

-- The persisted view, oldest first; ord is the line's first message, so a
-- double append collapses onto the row it already made.
CREATE TABLE IF NOT EXISTS view_lines (
	ord INTEGER PRIMARY KEY,
	l INTEGER NOT NULL,
	i INTEGER NOT NULL
);

-- The usage buckets date their sums: the chat bills the "main" anchor row,
-- whose single row cannot split by day, so every bill lands in one row per
-- UTC day, which /usage reads for today, the week and all time.
CREATE TABLE IF NOT EXISTS usage_days (
	day TEXT NOT NULL,
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cost REAL NOT NULL DEFAULT 0,
	PRIMARY KEY (day)
);

-- The billing and todos anchor of the chat: todo lists and a run's report
-- message foreign-key to a session, so the row exists before anything rides
-- it. INSERT OR IGNORE, so an existing store keeps its own.
INSERT OR IGNORE INTO sessions (id, parent_id, title, agent, created_at, updated_at)
	VALUES ('main', NULL, 'chat', '',
		CAST(strftime('%s', 'now') AS INTEGER) * 1000,
		CAST(strftime('%s', 'now') AS INTEGER) * 1000);
