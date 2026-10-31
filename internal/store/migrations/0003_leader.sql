-- +goose Up
-- The lease: the one window that may write this project's chat. A window
-- claims the row at boot when it is absent, renews its beat on the peers tick,
-- and a survivor takes it over once the beat goes stale or a clean close
-- releases it.
CREATE TABLE leader (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    origin TEXT NOT NULL,
    sock TEXT NOT NULL,
    beat INTEGER NOT NULL
);
