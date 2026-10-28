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
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    UNIQUE (principal_id, kind, idempotency_key)
);

CREATE TABLE upload_sessions (
    id               TEXT PRIMARY KEY,
    org_id           TEXT NOT NULL REFERENCES organizations (id),
    project_id       TEXT NOT NULL REFERENCES projects (id),
    created_by       TEXT NOT NULL,
    status           TEXT NOT NULL,
    file_count       BIGINT NOT NULL,
    total_bytes      BIGINT NOT NULL,
    chunk_bytes      BIGINT NOT NULL,
    max_chunks       BIGINT NOT NULL,
    exclude_patterns TEXT NOT NULL,
    operation_id     TEXT REFERENCES operations (id),
    snapshot_id      TEXT,
    expires_at       TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX upload_sessions_status ON upload_sessions (status, expires_at);

CREATE TABLE upload_chunks (
    upload_id TEXT NOT NULL REFERENCES upload_sessions (id),
    idx       INTEGER NOT NULL,
    size      BIGINT NOT NULL,
    PRIMARY KEY (upload_id, idx)
);

CREATE TABLE source_snapshots (
    id                TEXT PRIMARY KEY,
    org_id            TEXT NOT NULL REFERENCES organizations (id),
    project_id        TEXT NOT NULL REFERENCES projects (id),
    digest            TEXT NOT NULL,
    file_count        BIGINT NOT NULL,
    total_bytes       BIGINT NOT NULL,
    rejected_entries  BIGINT NOT NULL,
    excluded_entries  BIGINT NOT NULL,
    warnings          TEXT NOT NULL,
    created_by        TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX source_snapshots_project ON source_snapshots (project_id, id);

-- +goose StatementBegin
CREATE FUNCTION source_snapshots_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'source snapshots are immutable';
END;
$$;
CREATE TRIGGER source_snapshots_immutable BEFORE UPDATE ON source_snapshots
    FOR EACH ROW EXECUTE FUNCTION source_snapshots_immutable();
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON operations, upload_sessions, upload_chunks TO samuori_app;
        GRANT SELECT, INSERT, DELETE ON source_snapshots TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER source_snapshots_immutable ON source_snapshots;
DROP FUNCTION source_snapshots_immutable();
DROP TABLE source_snapshots;
DROP TABLE upload_chunks;
DROP TABLE upload_sessions;
DROP TABLE operations;
