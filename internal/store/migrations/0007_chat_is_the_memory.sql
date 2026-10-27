-- +goose Up
-- The chat is the memory: the brain tables go, and the main chat stopped
-- writing message rows, so checkpoints key on the log seq of the turn's user
-- message. The old rows keyed on message ids the log cannot name, so the
-- table starts over.
-- The live project-scope facts seed the log first, as note messages, so an
-- upgraded store keeps what it learned for the tree to summarize; a
-- superseded row is already replaced and goes with its table.
INSERT INTO log (seq, kind, text, meta_json, created_at)
	SELECT (SELECT COALESCE(MAX(seq), -1) FROM log) + ROW_NUMBER() OVER (ORDER BY updated_at, id),
		'note', text, '{"origin":' || json_quote(origin) || '}',
		strftime('%Y-%m-%dT%H:%M:%fZ', updated_at / 1000.0, 'unixepoch')
	FROM memory
	WHERE scope = 'project' AND superseded_by IS NULL;

-- every log row carries its view line, whatever wrote it
INSERT INTO view_lines (ord, l, i)
	SELECT seq, 0, seq FROM log
	WHERE NOT EXISTS (SELECT 1 FROM view_lines WHERE ord = log.seq);

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
