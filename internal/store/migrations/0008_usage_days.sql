-- +goose Up
-- The usage buckets date their sums: the chat bills the "main" anchor row,
-- whose single row cannot split by day, so every bill lands in one row per
-- UTC day, which /usage reads for today, the week and all time. The anchor's
-- accrual so far seeds today, so the buckets never read zero against a
-- session the screen shows.
CREATE TABLE IF NOT EXISTS usage_days (
	day TEXT NOT NULL,
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cost REAL NOT NULL DEFAULT 0,
	PRIMARY KEY (day)
);
INSERT OR IGNORE INTO usage_days (day, prompt_tokens, completion_tokens, cost)
	SELECT strftime('%Y-%m-%d', 'now'), prompt_tokens, completion_tokens, cost
	FROM sessions WHERE id = 'main' AND (prompt_tokens <> 0 OR completion_tokens <> 0 OR cost <> 0);

-- +goose Down
SELECT 1;
