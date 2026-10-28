-- +goose Up
-- Keys an administrator makes available to one project; its developers and maintainers bind them (secrets.md#grants).
CREATE TABLE secret_grants (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL REFERENCES organizations (id),
    project_id     TEXT NOT NULL REFERENCES projects (id),
    name           TEXT NOT NULL,
    provider_id    TEXT NOT NULL REFERENCES secret_providers (id),
    provider_ref   TEXT NOT NULL,
    version_policy TEXT NOT NULL,
    usage          TEXT NOT NULL CHECK (usage IN ('build', 'test', 'both')),
    pool_ids       TEXT NOT NULL,
    status         TEXT NOT NULL CHECK (status IN ('active', 'revoked')),
    reason         TEXT NOT NULL,
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    revoked_by     TEXT,
    revoked_at     TIMESTAMPTZ
);
CREATE UNIQUE INDEX secret_grants_active_name ON secret_grants (project_id, name) WHERE status = 'active';
ALTER TABLE secret_bindings ADD COLUMN grant_id TEXT REFERENCES secret_grants (id);

-- +goose Down
ALTER TABLE secret_bindings DROP COLUMN grant_id;
DROP TABLE secret_grants;
