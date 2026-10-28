-- +goose Up
CREATE TABLE controllers (
    id              TEXT PRIMARY KEY,
    hostname        TEXT NOT NULL,
    version         TEXT NOT NULL,
    started_at      TIMESTAMP NOT NULL,
    heartbeat_at    TIMESTAMP NOT NULL,
    stopped_at      TIMESTAMP
);

CREATE TABLE cluster_leases (
    name       TEXT PRIMARY KEY,
    holder     TEXT NOT NULL,
    epoch      BIGINT NOT NULL,
    expires_at TIMESTAMP NOT NULL
);

CREATE TABLE cluster_material (
    name       TEXT PRIMARY KEY,
    revision   BIGINT NOT NULL,
    key_id     TEXT NOT NULL,
    ciphertext BLOB NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    updated_by TEXT NOT NULL
);

CREATE TABLE worker_sessions (
    worker_id     TEXT PRIMARY KEY,
    controller_id TEXT NOT NULL,
    connected_at  TIMESTAMP NOT NULL
);

-- +goose Down
DROP TABLE worker_sessions;
DROP TABLE cluster_material;
DROP TABLE cluster_leases;
DROP TABLE controllers;
