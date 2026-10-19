-- +goose Up
CREATE TABLE IF NOT EXISTS sessions (
	id TEXT PRIMARY KEY,
	parent_id TEXT,
	title TEXT NOT NULL DEFAULT '',
	profile TEXT NOT NULL DEFAULT '',
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
	created_at INTEGER NOT NULL
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

CREATE TABLE IF NOT EXISTS checkpoints (
	message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	session_id TEXT NOT NULL,
	path TEXT NOT NULL,
	existed INTEGER NOT NULL,
	link INTEGER NOT NULL DEFAULT 0,
	content BLOB,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (message_id, path)
);
CREATE INDEX IF NOT EXISTS checkpoints_session ON checkpoints(session_id);

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

CREATE TABLE IF NOT EXISTS proposals (
	id TEXT PRIMARY KEY,
	payload TEXT NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS memory (
	id TEXT PRIMARY KEY,
	scope TEXT NOT NULL,
	text TEXT NOT NULL,
	quote TEXT NOT NULL DEFAULT '',
	provenance_session_id TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	superseded_by TEXT,
	origin TEXT NOT NULL
);
-- the trail group and /memory's merged list order by recency within a scope
CREATE INDEX IF NOT EXISTS memory_scope_updated ON memory(scope, updated_at);

CREATE TABLE IF NOT EXISTS memory_ops (
	op TEXT NOT NULL,
	id TEXT NOT NULL,
	ts INTEGER NOT NULL,
	text TEXT NOT NULL DEFAULT '',
	prior_text TEXT NOT NULL DEFAULT '',
	quote TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	hash TEXT NOT NULL DEFAULT '',
	superseded_by TEXT NOT NULL DEFAULT '',
	scope TEXT NOT NULL DEFAULT '',
	origin TEXT NOT NULL DEFAULT '',
	prior_bytes BLOB,
	prior_truncated INTEGER NOT NULL DEFAULT 0,
	prior_existed INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS memory_ops_id ON memory_ops(id);
