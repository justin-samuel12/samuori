-- +goose Up
-- An administrator may revoke an unused worker enrollment token before it expires.
ALTER TABLE enrollment_tokens ADD COLUMN revoked_at TIMESTAMP;
ALTER TABLE enrollment_tokens ADD COLUMN revoked_by TEXT;

-- +goose Down
ALTER TABLE enrollment_tokens DROP COLUMN revoked_by;
ALTER TABLE enrollment_tokens DROP COLUMN revoked_at;
