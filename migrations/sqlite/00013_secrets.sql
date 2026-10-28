-- +goose Up
CREATE TABLE secret_providers (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    name       TEXT NOT NULL,
    type       TEXT NOT NULL CHECK (type IN ('local', 'openbao', 'vault-compat', 'aws-secrets-manager')),
    created_at TIMESTAMP NOT NULL,
    UNIQUE (org_id, name)
);
CREATE UNIQUE INDEX secret_providers_local ON secret_providers (org_id) WHERE type = 'local';

CREATE TABLE secrets (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL REFERENCES organizations (id),
    name            TEXT NOT NULL,
    type            TEXT NOT NULL CHECK (type IN ('text', 'binary')),
    description     TEXT NOT NULL DEFAULT '',
    current_version INTEGER NOT NULL,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMP NOT NULL,
    updated_at      TIMESTAMP NOT NULL,
    UNIQUE (org_id, name)
);

CREATE TABLE secret_versions (
    secret_id  TEXT NOT NULL REFERENCES secrets (id),
    version    INTEGER NOT NULL,
    key_id     TEXT NOT NULL,
    nonce      BLOB NOT NULL,
    ciphertext BLOB NOT NULL,
    created_by TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    PRIMARY KEY (secret_id, version)
);

-- +goose StatementBegin
CREATE TRIGGER secret_versions_immutable BEFORE UPDATE ON secret_versions
BEGIN
    SELECT RAISE(ABORT, 'secret versions are immutable');
END;
-- +goose StatementEnd

CREATE TABLE secret_bindings (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL REFERENCES organizations (id),
    project_id     TEXT NOT NULL REFERENCES projects (id),
    logical_id     TEXT NOT NULL,
    provider_id    TEXT NOT NULL REFERENCES secret_providers (id),
    provider_ref   TEXT NOT NULL,
    version_policy TEXT NOT NULL,
    usage          TEXT NOT NULL CHECK (usage IN ('build', 'test', 'both')),
    pool_ids       TEXT NOT NULL,
    status         TEXT NOT NULL CHECK (status IN ('active', 'disabled')),
    version        INTEGER NOT NULL,
    reason         TEXT NOT NULL,
    expires_at     TIMESTAMP,
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMP NOT NULL,
    updated_by     TEXT NOT NULL,
    updated_at     TIMESTAMP NOT NULL
);
CREATE UNIQUE INDEX secret_bindings_active ON secret_bindings (project_id, logical_id) WHERE status = 'active';
CREATE INDEX secret_bindings_ref ON secret_bindings (provider_id, provider_ref);

-- +goose Down
DROP TABLE secret_bindings;
DROP TRIGGER secret_versions_immutable;
DROP TABLE secret_versions;
DROP TABLE secrets;
DROP TABLE secret_providers;
