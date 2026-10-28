-- +goose Up
-- One signed bundle per version, host facts for the checks before a rollout, and workers updated in waves per
-- pool (ADR 0045). Steps of earlier rollouts become one wave each, so they keep updating one host at a time.
CREATE TABLE release_bundles (
    version     TEXT PRIMARY KEY,
    manifest    BLOB NOT NULL,
    checksums   BLOB NOT NULL,
    signature   BLOB NOT NULL,
    size        INTEGER NOT NULL,
    uploaded_by TEXT NOT NULL,
    uploaded_at TIMESTAMP NOT NULL
);

ALTER TABLE controllers ADD COLUMN arch TEXT NOT NULL DEFAULT '';
ALTER TABLE controllers ADD COLUMN update_free_bytes INTEGER;
ALTER TABLE workers ADD COLUMN host_arch TEXT NOT NULL DEFAULT '';
ALTER TABLE workers ADD COLUMN update_free_bytes INTEGER;

ALTER TABLE rollouts ADD COLUMN paused INTEGER NOT NULL DEFAULT 0;
ALTER TABLE rollouts ADD COLUMN pause_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE rollouts ADD COLUMN wave_percent INTEGER NOT NULL DEFAULT 0;

CREATE TABLE rollout_steps_new (
    rollout_id TEXT NOT NULL REFERENCES rollouts (id),
    seq        INTEGER NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('controller', 'worker')),
    target     TEXT NOT NULL,
    name       TEXT NOT NULL,
    state      TEXT NOT NULL CHECK (state IN ('pending', 'draining', 'requested', 'applying', 'testing', 'rolling_back', 'succeeded', 'failed', 'skipped')),
    detail     TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMP NOT NULL,
    wave       INTEGER NOT NULL DEFAULT 0,
    pool_id    TEXT NOT NULL DEFAULT '',
    pool       TEXT NOT NULL DEFAULT '',
    location   TEXT NOT NULL DEFAULT '',
    arch       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (rollout_id, seq)
);
INSERT INTO rollout_steps_new (rollout_id, seq, kind, target, name, state, detail, updated_at, wave)
    SELECT rollout_id, seq, kind, target, name, state, detail, updated_at, seq FROM rollout_steps;
DROP TABLE rollout_steps;
ALTER TABLE rollout_steps_new RENAME TO rollout_steps;

-- +goose Down
DELETE FROM rollout_steps WHERE state = 'rolling_back';
CREATE TABLE rollout_steps_old (
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
INSERT INTO rollout_steps_old (rollout_id, seq, kind, target, name, state, detail, updated_at)
    SELECT rollout_id, seq, kind, target, name, state, detail, updated_at FROM rollout_steps;
DROP TABLE rollout_steps;
ALTER TABLE rollout_steps_old RENAME TO rollout_steps;
ALTER TABLE rollouts DROP COLUMN wave_percent;
ALTER TABLE rollouts DROP COLUMN pause_reason;
ALTER TABLE rollouts DROP COLUMN paused;
ALTER TABLE workers DROP COLUMN update_free_bytes;
ALTER TABLE workers DROP COLUMN host_arch;
ALTER TABLE controllers DROP COLUMN update_free_bytes;
ALTER TABLE controllers DROP COLUMN arch;
DROP TABLE release_bundles;
