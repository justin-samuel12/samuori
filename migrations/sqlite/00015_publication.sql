-- +goose Up
ALTER TABLE secrets ADD COLUMN controller_only INTEGER NOT NULL DEFAULT 0;

CREATE TABLE registries (
    id                    TEXT PRIMARY KEY,
    org_id                TEXT NOT NULL REFERENCES organizations (id),
    name                  TEXT NOT NULL,
    kind                  TEXT NOT NULL CHECK (kind IN ('oci', 'ecr', 'harbor')),
    endpoint              TEXT NOT NULL,
    plain_http            INTEGER NOT NULL,
    allow_private_network INTEGER NOT NULL,
    created_by            TEXT NOT NULL,
    created_at            TIMESTAMP NOT NULL,
    UNIQUE (org_id, name)
);

CREATE TABLE registry_profiles (
    id                TEXT PRIMARY KEY,
    org_id            TEXT NOT NULL REFERENCES organizations (id),
    registry_id       TEXT NOT NULL REFERENCES registries (id),
    name              TEXT NOT NULL,
    type              TEXT NOT NULL CHECK (type IN ('anonymous', 'oci-basic', 'oci-token', 'ecr')),
    purpose           TEXT NOT NULL CHECK (purpose IN ('pull', 'push')),
    username          TEXT NOT NULL,
    secret_id         TEXT REFERENCES secrets (id),
    repository_scopes TEXT NOT NULL,
    status            TEXT NOT NULL CHECK (status IN ('active', 'disabled')),
    last_test         TEXT,
    created_by        TEXT NOT NULL,
    created_at        TIMESTAMP NOT NULL,
    UNIQUE (org_id, name)
);

CREATE TABLE destinations (
    id                  TEXT PRIMARY KEY,
    org_id              TEXT NOT NULL REFERENCES organizations (id),
    name                TEXT NOT NULL,
    registry_id         TEXT NOT NULL REFERENCES registries (id),
    repository          TEXT NOT NULL,
    push_profile_id     TEXT NOT NULL REFERENCES registry_profiles (id),
    accepts_restricted  INTEGER NOT NULL,
    allowed_project_ids TEXT NOT NULL,
    protected_tags      TEXT NOT NULL,
    immutable_tags      INTEGER NOT NULL,
    publishing_id       TEXT,
    created_by          TEXT NOT NULL,
    created_at          TIMESTAMP NOT NULL,
    UNIQUE (org_id, name)
);

CREATE TABLE publications (
    id                 TEXT PRIMARY KEY,
    org_id             TEXT NOT NULL REFERENCES organizations (id),
    project_id         TEXT NOT NULL REFERENCES projects (id),
    kind               TEXT NOT NULL CHECK (kind IN ('publish', 'promote')),
    artifact_id        TEXT NOT NULL REFERENCES build_artifacts (id),
    artifact_digest    TEXT NOT NULL,
    destination_id     TEXT NOT NULL REFERENCES destinations (id),
    tags               TEXT NOT NULL,
    state              TEXT NOT NULL,
    reason             TEXT,
    gate_decisions     TEXT NOT NULL,
    image_digest       TEXT,
    destination_ref    TEXT,
    requested_by       TEXT NOT NULL,
    idempotency_key    TEXT NOT NULL,
    created_at         TIMESTAMP NOT NULL,
    updated_at         TIMESTAMP NOT NULL,
    UNIQUE (requested_by, idempotency_key)
);
CREATE INDEX publications_state ON publications (state);
CREATE INDEX publications_artifact ON publications (artifact_id);

-- +goose Down
DROP TABLE publications;
DROP TABLE destinations;
DROP TABLE registry_profiles;
DROP TABLE registries;
ALTER TABLE secrets DROP COLUMN controller_only;
