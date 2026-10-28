-- +goose Up
-- The first administrator's email, given to samuori-setup (ADR 0045).
ALTER TABLE users ADD COLUMN email TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE users DROP COLUMN email;
