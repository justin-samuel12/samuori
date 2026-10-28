-- +goose Up
-- Signed releases administrators uploaded, and rollouts that update controllers, then workers (ADR 0044).
CREATE TABLE releases (
    version     TEXT NOT NULL,
    arch        TEXT NOT NULL,
    digest      TEXT NOT NULL,
    size        INTEGER NOT NULL,
    checksums   BLOB NOT NULL,
    signature   BLOB NOT NULL,
    uploaded_by TEXT NOT NULL,
    uploaded_at TIMESTAMP NOT NULL,
    PRIMARY KEY (version, arch)
);

CREATE TABLE rollouts (
    id          TEXT PRIMARY KEY,
    version     TEXT NOT NULL,
    state       TEXT NOT NULL CHECK (state IN ('running', 'succeeded', 'failed', 'cancelled')),
    reason      TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMP NOT NULL,
    finished_at TIMESTAMP
);

CREATE TABLE rollout_steps (
    rollout_id TEXT NOT NULL REFERENCES rollouts (id),
    seq        INTEGER NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('controller', 'worker')),
    target     TEXT NOT NULL,
    name       TEXT NOT NULL,
    state      TEXT NOT NULL CHECK (state IN ('pending', 'draining', 'requested', 'applying', 'testing', 'succeeded', 'failed', 'skipped')),
    detail     TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMP NOT NULL,
    PRIMARY KEY (rollout_id, seq)
);

-- +goose Down
DROP TABLE rollout_steps;
DROP TABLE rollouts;
DROP TABLE releases;
