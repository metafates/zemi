-- +goose Up
-- The column is really "which agent ran": it stores subagent run names too.
ALTER TABLE sessions RENAME COLUMN profile TO agent;
DELETE FROM kv WHERE key = 'config:agent.profile';

-- +goose Down
ALTER TABLE sessions RENAME COLUMN agent TO profile;
