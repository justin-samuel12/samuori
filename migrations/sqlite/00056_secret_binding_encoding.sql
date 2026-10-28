-- +goose Up
ALTER TABLE secret_bindings ADD COLUMN value_encoding TEXT NOT NULL DEFAULT '' CHECK (value_encoding IN ('', 'base64'));

-- +goose Down
ALTER TABLE secret_bindings DROP COLUMN value_encoding;
