-- +goose Up
ALTER TABLE secret_providers ADD COLUMN config TEXT NOT NULL DEFAULT '{}';
ALTER TABLE secret_providers ADD COLUMN credential_secret_id TEXT;
ALTER TABLE secret_providers ADD COLUMN last_check TEXT;

-- +goose Down
ALTER TABLE secret_providers DROP COLUMN last_check;
ALTER TABLE secret_providers DROP COLUMN credential_secret_id;
ALTER TABLE secret_providers DROP COLUMN config;
