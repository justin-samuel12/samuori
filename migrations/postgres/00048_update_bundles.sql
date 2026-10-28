-- +goose Up
-- One signed bundle per version, host facts for the checks before a rollout, and workers updated in waves per
-- pool (ADR 0045). Steps of earlier rollouts become one wave each, so they keep updating one host at a time.
CREATE TABLE release_bundles (
    version     TEXT PRIMARY KEY,
    manifest    BYTEA NOT NULL,
    checksums   BYTEA NOT NULL,
    signature   BYTEA NOT NULL,
    size        BIGINT NOT NULL,
    uploaded_by TEXT NOT NULL,
    uploaded_at TIMESTAMPTZ NOT NULL
);

ALTER TABLE controllers ADD COLUMN arch TEXT NOT NULL DEFAULT '';
ALTER TABLE controllers ADD COLUMN update_free_bytes BIGINT;
ALTER TABLE workers ADD COLUMN host_arch TEXT NOT NULL DEFAULT '';
ALTER TABLE workers ADD COLUMN update_free_bytes BIGINT;

ALTER TABLE rollouts ADD COLUMN paused BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE rollouts ADD COLUMN pause_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE rollouts ADD COLUMN wave_percent INTEGER NOT NULL DEFAULT 0;

ALTER TABLE rollout_steps DROP CONSTRAINT rollout_steps_state_check;
ALTER TABLE rollout_steps ADD CONSTRAINT rollout_steps_state_check
    CHECK (state IN ('pending', 'draining', 'requested', 'applying', 'testing', 'rolling_back', 'succeeded', 'failed', 'skipped'));
ALTER TABLE rollout_steps ADD COLUMN wave INTEGER NOT NULL DEFAULT 0;
ALTER TABLE rollout_steps ADD COLUMN pool_id TEXT NOT NULL DEFAULT '';
ALTER TABLE rollout_steps ADD COLUMN pool TEXT NOT NULL DEFAULT '';
ALTER TABLE rollout_steps ADD COLUMN location TEXT NOT NULL DEFAULT '';
ALTER TABLE rollout_steps ADD COLUMN arch TEXT NOT NULL DEFAULT '';
UPDATE rollout_steps SET wave = seq;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON release_bundles TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DELETE FROM rollout_steps WHERE state = 'rolling_back';
ALTER TABLE rollout_steps DROP COLUMN arch;
ALTER TABLE rollout_steps DROP COLUMN location;
ALTER TABLE rollout_steps DROP COLUMN pool;
ALTER TABLE rollout_steps DROP COLUMN pool_id;
ALTER TABLE rollout_steps DROP COLUMN wave;
ALTER TABLE rollout_steps DROP CONSTRAINT rollout_steps_state_check;
ALTER TABLE rollout_steps ADD CONSTRAINT rollout_steps_state_check
    CHECK (state IN ('pending', 'draining', 'requested', 'applying', 'testing', 'succeeded', 'failed', 'skipped'));
ALTER TABLE rollouts DROP COLUMN wave_percent;
ALTER TABLE rollouts DROP COLUMN pause_reason;
ALTER TABLE rollouts DROP COLUMN paused;
ALTER TABLE workers DROP COLUMN update_free_bytes;
ALTER TABLE workers DROP COLUMN host_arch;
ALTER TABLE controllers DROP COLUMN update_free_bytes;
ALTER TABLE controllers DROP COLUMN arch;
DROP TABLE release_bundles;
