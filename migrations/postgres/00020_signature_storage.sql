-- +goose Up
ALTER TABLE publications ADD COLUMN signature_storage TEXT;

-- +goose Down
ALTER TABLE publications DROP COLUMN signature_storage;
