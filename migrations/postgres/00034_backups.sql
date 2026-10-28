-- +goose Up
CREATE TABLE backups (
    id             TEXT PRIMARY KEY,
    created_at     TIMESTAMPTZ NOT NULL,
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
    checked_at     TIMESTAMPTZ,
    check_result   TEXT,
    removed_at     TIMESTAMPTZ
);
CREATE INDEX backups_created ON backups (created_at);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON backups TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE backups;
