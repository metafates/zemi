-- +goose Up
-- The eternal chat: one append-only log per project DB, the binary tree of
-- one-line summaries built over it, and the persisted view. seq is the
-- 0-based message index, assigned max+1 inside the append transaction, never
-- AUTOINCREMENT: a rewind's tail cut must leave it dense.
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

-- The billing and todos anchor of the new world; nothing reads it yet.
INSERT OR IGNORE INTO sessions (id, parent_id, title, agent, created_at, updated_at)
	VALUES ('main', NULL, 'chat', '',
		CAST(strftime('%s', 'now') AS INTEGER) * 1000,
		CAST(strftime('%s', 'now') AS INTEGER) * 1000);

-- +goose Down
DELETE FROM sessions WHERE id = 'main';
DROP TABLE view_lines;
DROP TABLE nodes;
DROP TABLE log;
