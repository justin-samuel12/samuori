-- +goose Up
-- The secrets of external stores an administrator allows Samuori to grant (docs/architecture/secrets.md#allowed-secrets).
CREATE TABLE allowed_secrets (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL,
    provider_id TEXT NOT NULL REFERENCES secret_providers (id),
    reference   TEXT NOT NULL,
    label       TEXT NOT NULL,
    note        TEXT NOT NULL DEFAULT '',
    fields      TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('active', 'removed')),
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMP NOT NULL,
    removed_by  TEXT,
    removed_at  TIMESTAMP
);
CREATE UNIQUE INDEX allowed_secrets_active ON allowed_secrets (provider_id, reference) WHERE status = 'active';
ALTER TABLE secret_grants ADD COLUMN allowed_secret_id TEXT;

-- +goose Down
ALTER TABLE secret_grants DROP COLUMN allowed_secret_id;
DROP TABLE allowed_secrets;
