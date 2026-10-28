-- +goose Up
CREATE TABLE identity_providers (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL REFERENCES organizations (id),
    kind         TEXT NOT NULL CHECK (kind IN ('oidc', 'ldap')),
    name         TEXT NOT NULL,
    display_name TEXT NOT NULL,
    enabled      BOOLEAN NOT NULL DEFAULT FALSE,
    config       TEXT NOT NULL,
    secret_id    TEXT,
    created_by   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    UNIQUE (org_id, name)
);

CREATE TABLE identity_group_mappings (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations (id),
    provider_id TEXT NOT NULL REFERENCES identity_providers (id),
    group_name  TEXT NOT NULL,
    role        TEXT NOT NULL,
    scope_type  TEXT NOT NULL,
    scope_id    TEXT NOT NULL,
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    UNIQUE (provider_id, group_name, role, scope_type, scope_id)
);

CREATE TABLE oidc_logins (
    state_hash   TEXT PRIMARY KEY,
    provider_id  TEXT NOT NULL REFERENCES identity_providers (id),
    browser_hash TEXT NOT NULL,
    nonce        TEXT NOT NULL,
    verifier     TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    used_at      TIMESTAMPTZ
);

ALTER TABLE users ADD COLUMN kind TEXT NOT NULL DEFAULT 'user';
ALTER TABLE users ADD COLUMN provider_id TEXT;
ALTER TABLE users ADD COLUMN external_id TEXT;
CREATE UNIQUE INDEX users_external ON users (provider_id, external_id) WHERE provider_id IS NOT NULL;
ALTER TABLE role_bindings ADD COLUMN source_provider_id TEXT;
ALTER TABLE organizations ADD COLUMN local_login TEXT NOT NULL DEFAULT 'all';

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON identity_providers, identity_group_mappings, oidc_logins TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
ALTER TABLE organizations DROP COLUMN local_login;
ALTER TABLE role_bindings DROP COLUMN source_provider_id;
DROP INDEX users_external;
ALTER TABLE users DROP COLUMN external_id;
ALTER TABLE users DROP COLUMN provider_id;
ALTER TABLE users DROP COLUMN kind;
DROP TABLE oidc_logins;
DROP TABLE identity_group_mappings;
DROP TABLE identity_providers;
