-- +goose Up
ALTER TABLE signing_keys ADD COLUMN backend TEXT NOT NULL DEFAULT 'local';
ALTER TABLE signing_keys ADD COLUMN provider_id TEXT;
ALTER TABLE signing_keys ADD COLUMN key_ref TEXT;

-- +goose Down
ALTER TABLE signing_keys DROP COLUMN key_ref;
ALTER TABLE signing_keys DROP COLUMN provider_id;
ALTER TABLE signing_keys DROP COLUMN backend;
