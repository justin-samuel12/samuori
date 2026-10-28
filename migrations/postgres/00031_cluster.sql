-- +goose Up
CREATE TABLE controllers (
    id              TEXT PRIMARY KEY,
    hostname        TEXT NOT NULL,
    version         TEXT NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL,
    heartbeat_at    TIMESTAMPTZ NOT NULL,
    stopped_at      TIMESTAMPTZ
);

CREATE TABLE cluster_leases (
    name       TEXT PRIMARY KEY,
    holder     TEXT NOT NULL,
    epoch      BIGINT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE cluster_material (
    name       TEXT PRIMARY KEY,
    revision   BIGINT NOT NULL,
    key_id     TEXT NOT NULL,
    ciphertext BYTEA NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    updated_by TEXT NOT NULL
);

CREATE TABLE worker_sessions (
    worker_id     TEXT PRIMARY KEY,
    controller_id TEXT NOT NULL,
    connected_at  TIMESTAMPTZ NOT NULL
);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON controllers, cluster_leases, cluster_material, worker_sessions TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE worker_sessions;
DROP TABLE cluster_material;
DROP TABLE cluster_leases;
DROP TABLE controllers;
