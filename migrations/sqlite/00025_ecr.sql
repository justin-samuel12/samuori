-- +goose Up
ALTER TABLE registries ADD COLUMN account_id TEXT NOT NULL DEFAULT '';
ALTER TABLE registries ADD COLUMN region TEXT NOT NULL DEFAULT '';
ALTER TABLE registry_profiles ADD COLUMN config TEXT NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE registry_profiles DROP COLUMN config;
ALTER TABLE registries DROP COLUMN region;
ALTER TABLE registries DROP COLUMN account_id;
