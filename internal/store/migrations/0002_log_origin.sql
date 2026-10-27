-- +goose Up
-- Which window wrote the row: two windows share one repository's log, and
-- the live pairing must tell their rows apart to splice the other window's
-- rows into its own transcript at their place. '' is anything written before
-- the column, whose windows are gone.
ALTER TABLE log ADD COLUMN origin TEXT NOT NULL DEFAULT '';
