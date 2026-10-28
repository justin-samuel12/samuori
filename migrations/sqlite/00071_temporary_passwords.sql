-- +goose Up
-- Set when an administrator resets a password: the password is temporary and must be replaced before then (ADR 0065).
ALTER TABLE user_credentials ADD COLUMN temporary_until TIMESTAMP;

-- +goose Down
ALTER TABLE user_credentials DROP COLUMN temporary_until;
