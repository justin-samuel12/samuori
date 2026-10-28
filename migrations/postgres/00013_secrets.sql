-- +goose Up
CREATE TABLE secret_providers (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    name       TEXT NOT NULL,
    type       TEXT NOT NULL CHECK (type IN ('local', 'openbao', 'vault-compat', 'aws-secrets-manager')),
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (org_id, name)
);
CREATE UNIQUE INDEX secret_providers_local ON secret_providers (org_id) WHERE type = 'local';

CREATE TABLE secrets (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL REFERENCES organizations (id),
    name            TEXT NOT NULL,
    type            TEXT NOT NULL CHECK (type IN ('text', 'binary')),
    description     TEXT NOT NULL DEFAULT '',
    current_version BIGINT NOT NULL,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    UNIQUE (org_id, name)
);

CREATE TABLE secret_versions (
    secret_id  TEXT NOT NULL REFERENCES secrets (id),
    version    BIGINT NOT NULL,
    key_id     TEXT NOT NULL,
    nonce      BYTEA NOT NULL,
    ciphertext BYTEA NOT NULL,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (secret_id, version)
);

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
    version        BIGINT NOT NULL,
    reason         TEXT NOT NULL,
    expires_at     TIMESTAMPTZ,
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    updated_by     TEXT NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX secret_bindings_active ON secret_bindings (project_id, logical_id) WHERE status = 'active';
CREATE INDEX secret_bindings_ref ON secret_bindings (provider_id, provider_ref);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE ON secret_providers, secrets, secret_bindings TO samuori_app;
        GRANT SELECT, INSERT ON secret_versions TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE secret_bindings;
DROP TABLE secret_versions;
DROP TABLE secrets;
DROP TABLE secret_providers;
