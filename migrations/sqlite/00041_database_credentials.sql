-- +goose Up
-- PostgreSQL password revisions (ADR 0043); kept in SQLite too so both dialects share one schema version.
CREATE TABLE database_credentials (
    revision   INTEGER PRIMARY KEY,
    sealed     BLOB NOT NULL,
    reason     TEXT NOT NULL,
    created_by TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL
);

CREATE TABLE database_credential_checks (
    revision      INTEGER NOT NULL REFERENCES database_credentials (revision),
    controller_id TEXT NOT NULL,
    hostname      TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('verified', 'failed')),
    detail        TEXT NOT NULL DEFAULT '',
    checked_at    TIMESTAMP NOT NULL,
    PRIMARY KEY (revision, controller_id)
);

-- +goose Down
DROP TABLE database_credential_checks;
DROP TABLE database_credentials;
