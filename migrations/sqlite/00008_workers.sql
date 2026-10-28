-- +goose Up
CREATE TABLE worker_pools (
    id                     TEXT PRIMARY KEY,
    org_id                 TEXT NOT NULL REFERENCES organizations (id),
    name                   TEXT NOT NULL,
    location               TEXT NOT NULL,
    isolation_profile      TEXT NOT NULL,
    trust_domain           TEXT NOT NULL,
    trusted_workloads_only INTEGER NOT NULL,
    profile_approved       INTEGER NOT NULL,
    created_at             TIMESTAMP NOT NULL,
    UNIQUE (org_id, name)
);

CREATE TABLE enrollment_tokens (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    pool_id    TEXT NOT NULL REFERENCES worker_pools (id),
    token_hash TEXT NOT NULL UNIQUE,
    created_by TEXT NOT NULL,
    expires_at TIMESTAMP NOT NULL,
    used_at    TIMESTAMP,
    worker_id  TEXT,
    created_at TIMESTAMP NOT NULL
);

CREATE TABLE workers (
    id                TEXT PRIMARY KEY,
    org_id            TEXT NOT NULL REFERENCES organizations (id),
    pool_id           TEXT NOT NULL REFERENCES worker_pools (id),
    name              TEXT NOT NULL,
    enrollment_state  TEXT NOT NULL,
    operational_state TEXT NOT NULL,
    cert_serial       TEXT NOT NULL,
    cert_expires_at   TIMESTAMP NOT NULL,
    capabilities      TEXT NOT NULL,
    version           TEXT NOT NULL,
    protocol_minor    INTEGER NOT NULL,
    last_seen_at      TIMESTAMP,
    created_at        TIMESTAMP NOT NULL
);
CREATE INDEX workers_pool ON workers (pool_id);

-- +goose Down
DROP TABLE workers;
DROP TABLE enrollment_tokens;
DROP TABLE worker_pools;
