-- +goose Up
-- The shared-chat lease went with multi-window support: one zemi per project
-- now, so no row names a writing window.
DROP TABLE IF EXISTS leader;
