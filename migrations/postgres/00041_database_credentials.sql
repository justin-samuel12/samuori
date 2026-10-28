-- +goose Up
-- PostgreSQL passwords an administrator entered after changing them at the database, sealed with the cluster key
-- (ADR 0043). Each controller verifies a revision before switching to it and records the result.
CREATE TABLE database_credentials (
    revision   BIGINT PRIMARY KEY,
    sealed     BYTEA NOT NULL,
    reason     TEXT NOT NULL,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE database_credential_checks (
    revision      BIGINT NOT NULL REFERENCES database_credentials (revision),
    controller_id TEXT NOT NULL,
    hostname      TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('verified', 'failed')),
    detail        TEXT NOT NULL DEFAULT '',
    checked_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (revision, controller_id)
);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON database_credentials, database_credential_checks TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE database_credential_checks;
DROP TABLE database_credentials;
