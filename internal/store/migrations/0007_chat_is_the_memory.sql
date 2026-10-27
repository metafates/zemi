-- +goose Up
-- The chat is the memory: the brain tables go, and the main chat stopped
-- writing message rows, so checkpoints key on the log seq of the turn's user
-- message. The old rows keyed on message ids the log cannot name, so the
-- table starts over.
DROP TABLE IF EXISTS proposals;
DROP TABLE IF EXISTS memory;
DROP TABLE IF EXISTS memory_ops;

DROP TABLE IF EXISTS checkpoints;
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

-- +goose Down
SELECT 1;
