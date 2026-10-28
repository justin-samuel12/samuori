-- +goose Up
CREATE TABLE organizations (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE teams (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (org_id, name)
);

CREATE TABLE projects (
    id               TEXT PRIMARY KEY,
    org_id           TEXT NOT NULL REFERENCES organizations (id),
    team_id          TEXT NOT NULL REFERENCES teams (id),
    name             TEXT NOT NULL,
    publication_mode TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,
    UNIQUE (team_id, name)
);

CREATE TABLE users (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL REFERENCES organizations (id),
    username     TEXT NOT NULL,
    display_name TEXT NOT NULL,
    disabled     BOOLEAN NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    UNIQUE (org_id, username)
);

CREATE TABLE role_bindings (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL REFERENCES organizations (id),
    principal_id TEXT NOT NULL,
    role         TEXT NOT NULL,
    scope_type   TEXT NOT NULL,
    scope_id     TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    UNIQUE (principal_id, role, scope_type, scope_id)
);
CREATE INDEX role_bindings_principal ON role_bindings (principal_id);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON organizations, teams, projects, users, role_bindings TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE role_bindings;
DROP TABLE users;
DROP TABLE projects;
DROP TABLE teams;
DROP TABLE organizations;
