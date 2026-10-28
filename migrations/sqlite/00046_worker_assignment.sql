-- +goose Up
-- Workers enrol unassigned and are assigned to a pool later; portal tokens name no pool (ADR 0045).
-- SQLite cannot drop NOT NULL, so workers and enrollment_tokens are rebuilt; worker_removals references workers
-- and is rebuilt around them so foreign keys stay enforced inside the transaction.
CREATE TABLE workers_new (
    id                TEXT PRIMARY KEY,
    org_id            TEXT NOT NULL REFERENCES organizations (id),
    pool_id           TEXT REFERENCES worker_pools (id),
    name              TEXT NOT NULL,
    enrollment_state  TEXT NOT NULL,
    operational_state TEXT NOT NULL,
    cert_serial       TEXT NOT NULL,
    cert_expires_at   TIMESTAMP NOT NULL,
    capabilities      TEXT NOT NULL,
    version           TEXT NOT NULL,
    protocol_minor    INTEGER NOT NULL,
    last_seen_at      TIMESTAMP,
    created_at        TIMESTAMP NOT NULL,
    prev_cert_serial  TEXT NOT NULL DEFAULT '',
    bundle_version    INTEGER NOT NULL DEFAULT 0,
    revoked_at        TIMESTAMP,
    revoke_reason     TEXT NOT NULL DEFAULT '',
    compromised_since TIMESTAMP,
    replaced_by       TEXT NOT NULL DEFAULT ''
);
INSERT INTO workers_new (id, org_id, pool_id, name, enrollment_state, operational_state, cert_serial, cert_expires_at, capabilities, version, protocol_minor, last_seen_at, created_at, prev_cert_serial, bundle_version, revoked_at, revoke_reason, compromised_since, replaced_by)
    SELECT id, org_id, pool_id, name, enrollment_state, operational_state, cert_serial, cert_expires_at, capabilities, version, protocol_minor, last_seen_at, created_at, prev_cert_serial, bundle_version, revoked_at, revoke_reason, compromised_since, replaced_by FROM workers;
CREATE TABLE worker_removals_keep AS SELECT * FROM worker_removals;
DROP TABLE worker_removals;
DROP TABLE workers;
ALTER TABLE workers_new RENAME TO workers;
CREATE INDEX workers_pool ON workers (pool_id);
CREATE TABLE worker_removals (
    worker_id    TEXT PRIMARY KEY REFERENCES workers (id),
    org_id       TEXT NOT NULL,
    reason       TEXT NOT NULL,
    requested_by TEXT NOT NULL,
    requested_at TIMESTAMP NOT NULL,
    state        TEXT NOT NULL CHECK (state IN ('draining', 'wiping', 'removed')),
    outcome      TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('', 'wiped', 'not-confirmed')),
    detail       TEXT NOT NULL DEFAULT '',
    updated_at   TIMESTAMP NOT NULL
);
INSERT INTO worker_removals (worker_id, org_id, reason, requested_by, requested_at, state, outcome, detail, updated_at)
    SELECT worker_id, org_id, reason, requested_by, requested_at, state, outcome, detail, updated_at FROM worker_removals_keep;
DROP TABLE worker_removals_keep;

CREATE TABLE enrollment_tokens_new (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    pool_id    TEXT REFERENCES worker_pools (id),
    token_hash TEXT NOT NULL UNIQUE,
    created_by TEXT NOT NULL,
    expires_at TIMESTAMP NOT NULL,
    used_at    TIMESTAMP,
    worker_id  TEXT,
    created_at TIMESTAMP NOT NULL
);
INSERT INTO enrollment_tokens_new (id, org_id, pool_id, token_hash, created_by, expires_at, used_at, worker_id, created_at)
    SELECT id, org_id, pool_id, token_hash, created_by, expires_at, used_at, worker_id, created_at FROM enrollment_tokens;
DROP TABLE enrollment_tokens;
ALTER TABLE enrollment_tokens_new RENAME TO enrollment_tokens;

-- Every organization starts with one location, default, unless it already has locations.
INSERT INTO locations (id, org_id, name, description, created_at)
    SELECT 'loc-' || o.id || '-default', o.id, 'default', '', o.created_at FROM organizations o
    WHERE NOT EXISTS (SELECT 1 FROM locations l WHERE l.org_id = o.id);

-- +goose Down
DELETE FROM worker_removals WHERE worker_id IN (SELECT id FROM workers WHERE pool_id IS NULL);
DELETE FROM workers WHERE pool_id IS NULL;
DELETE FROM enrollment_tokens WHERE pool_id IS NULL;
CREATE TABLE workers_old (
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
    created_at        TIMESTAMP NOT NULL,
    prev_cert_serial  TEXT NOT NULL DEFAULT '',
    bundle_version    INTEGER NOT NULL DEFAULT 0,
    revoked_at        TIMESTAMP,
    revoke_reason     TEXT NOT NULL DEFAULT '',
    compromised_since TIMESTAMP,
    replaced_by       TEXT NOT NULL DEFAULT ''
);
INSERT INTO workers_old (id, org_id, pool_id, name, enrollment_state, operational_state, cert_serial, cert_expires_at, capabilities, version, protocol_minor, last_seen_at, created_at, prev_cert_serial, bundle_version, revoked_at, revoke_reason, compromised_since, replaced_by)
    SELECT id, org_id, pool_id, name, enrollment_state, operational_state, cert_serial, cert_expires_at, capabilities, version, protocol_minor, last_seen_at, created_at, prev_cert_serial, bundle_version, revoked_at, revoke_reason, compromised_since, replaced_by FROM workers;
CREATE TABLE worker_removals_keep AS SELECT * FROM worker_removals;
DROP TABLE worker_removals;
DROP TABLE workers;
ALTER TABLE workers_old RENAME TO workers;
CREATE INDEX workers_pool ON workers (pool_id);
CREATE TABLE worker_removals (
    worker_id    TEXT PRIMARY KEY REFERENCES workers (id),
    org_id       TEXT NOT NULL,
    reason       TEXT NOT NULL,
    requested_by TEXT NOT NULL,
    requested_at TIMESTAMP NOT NULL,
    state        TEXT NOT NULL CHECK (state IN ('draining', 'wiping', 'removed')),
    outcome      TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('', 'wiped', 'not-confirmed')),
    detail       TEXT NOT NULL DEFAULT '',
    updated_at   TIMESTAMP NOT NULL
);
INSERT INTO worker_removals (worker_id, org_id, reason, requested_by, requested_at, state, outcome, detail, updated_at)
    SELECT worker_id, org_id, reason, requested_by, requested_at, state, outcome, detail, updated_at FROM worker_removals_keep;
DROP TABLE worker_removals_keep;
CREATE TABLE enrollment_tokens_old (
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
INSERT INTO enrollment_tokens_old (id, org_id, pool_id, token_hash, created_by, expires_at, used_at, worker_id, created_at)
    SELECT id, org_id, pool_id, token_hash, created_by, expires_at, used_at, worker_id, created_at FROM enrollment_tokens;
DROP TABLE enrollment_tokens;
ALTER TABLE enrollment_tokens_old RENAME TO enrollment_tokens;
