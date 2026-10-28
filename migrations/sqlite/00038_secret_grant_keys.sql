-- +goose Up
-- A grant of a whole KV v2 secret records its key names, never values (secrets.md#grants).
ALTER TABLE secret_grants ADD COLUMN fields TEXT NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE secret_grants DROP COLUMN fields;
