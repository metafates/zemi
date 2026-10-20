-- +goose Up
-- A message's own rows: the facts its text cannot say, so a resumed session
-- rebuilds the same rows (a call's status, its job or run).
ALTER TABLE messages ADD COLUMN row_meta TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE messages DROP COLUMN row_meta;
