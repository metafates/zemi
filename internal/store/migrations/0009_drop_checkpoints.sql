-- +goose Up
-- Rewind is gone: the chat never ends, and /reset is the only wipe. The
-- pre-turn file snapshots it restored have no reader left.

DROP TABLE IF EXISTS checkpoints;

-- +goose Down
CREATE TABLE IF NOT EXISTS checkpoints (
	seq INTEGER NOT NULL,
	path TEXT NOT NULL,
	existed INTEGER NOT NULL,
	link INTEGER NOT NULL DEFAULT 0,
	content BLOB,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (seq, path)
);
CREATE INDEX IF NOT EXISTS checkpoints_seq ON checkpoints(seq);
