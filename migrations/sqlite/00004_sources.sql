-- +goose Up
CREATE TABLE operations (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL REFERENCES organizations (id),
    principal_id    TEXT NOT NULL,
    kind            TEXT NOT NULL,
    status          TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    resource_type   TEXT,
    resource_id     TEXT,
    error_code      TEXT,
    error_message   TEXT,
    created_at      TIMESTAMP NOT NULL,
    updated_at      TIMESTAMP NOT NULL,
    UNIQUE (principal_id, kind, idempotency_key)
);

CREATE TABLE upload_sessions (
    id               TEXT PRIMARY KEY,
    org_id           TEXT NOT NULL REFERENCES organizations (id),
    project_id       TEXT NOT NULL REFERENCES projects (id),
    created_by       TEXT NOT NULL,
    status           TEXT NOT NULL,
    file_count       INTEGER NOT NULL,
    total_bytes      INTEGER NOT NULL,
    chunk_bytes      INTEGER NOT NULL,
    max_chunks       INTEGER NOT NULL,
    exclude_patterns TEXT NOT NULL,
    operation_id     TEXT REFERENCES operations (id),
    snapshot_id      TEXT,
    expires_at       TIMESTAMP NOT NULL,
    created_at       TIMESTAMP NOT NULL
);
CREATE INDEX upload_sessions_status ON upload_sessions (status, expires_at);

CREATE TABLE upload_chunks (
    upload_id TEXT NOT NULL REFERENCES upload_sessions (id),
    idx       INTEGER NOT NULL,
    size      INTEGER NOT NULL,
    PRIMARY KEY (upload_id, idx)
);

CREATE TABLE source_snapshots (
    id                TEXT PRIMARY KEY,
    org_id            TEXT NOT NULL REFERENCES organizations (id),
    project_id        TEXT NOT NULL REFERENCES projects (id),
    digest            TEXT NOT NULL,
    file_count        INTEGER NOT NULL,
    total_bytes       INTEGER NOT NULL,
    rejected_entries  INTEGER NOT NULL,
    excluded_entries  INTEGER NOT NULL,
    warnings          TEXT NOT NULL,
    created_by        TEXT NOT NULL,
    created_at        TIMESTAMP NOT NULL
);
CREATE INDEX source_snapshots_project ON source_snapshots (project_id, id);

-- +goose StatementBegin
CREATE TRIGGER source_snapshots_immutable BEFORE UPDATE ON source_snapshots
BEGIN
    SELECT RAISE(ABORT, 'source snapshots are immutable');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER source_snapshots_immutable;
DROP TABLE source_snapshots;
DROP TABLE upload_chunks;
DROP TABLE upload_sessions;
DROP TABLE operations;
