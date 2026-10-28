-- +goose Up
CREATE TABLE backups (
    id             TEXT PRIMARY KEY,
    created_at     TIMESTAMP NOT NULL,
    file           TEXT NOT NULL,
    size           BIGINT NOT NULL,
    sha256         TEXT NOT NULL,
    dialect        TEXT NOT NULL,
    schema_version BIGINT NOT NULL,
    tables         INTEGER NOT NULL,
    row_count      BIGINT NOT NULL,
    blobs          INTEGER NOT NULL,
    includes_keys  BOOLEAN NOT NULL,
    trigger        TEXT NOT NULL,
    created_by     TEXT,
    checked_at     TIMESTAMP,
    check_result   TEXT,
    removed_at     TIMESTAMP
);
CREATE INDEX backups_created ON backups (created_at);

-- +goose Down
DROP TABLE backups;
