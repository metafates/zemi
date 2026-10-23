-- +goose Up
CREATE INDEX IF NOT EXISTS memory_ops_session ON memory_ops(session_id);

-- +goose Down
DROP INDEX memory_ops_session;
