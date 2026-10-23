-- +goose Up
CREATE INDEX IF NOT EXISTS checkpoints_created ON checkpoints(created_at);

-- +goose Down
DROP INDEX checkpoints_created;
