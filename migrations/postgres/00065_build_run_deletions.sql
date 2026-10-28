-- +goose Up
-- A deleted build keeps its run, attempts, validations and audit trail; garbage collection stops keeping its image,
-- logs and cache (ADR 0053).
CREATE TABLE build_run_deletions (
    run_id     TEXT PRIMARY KEY REFERENCES build_runs (id),
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    deleted_by TEXT NOT NULL,
    reason     TEXT NOT NULL,
    deleted_at TIMESTAMPTZ NOT NULL
);
-- 00063 and 00064 used TIMESTAMP; every value written was UTC.
ALTER TABLE controllers ALTER COLUMN capacity_at TYPE TIMESTAMPTZ USING capacity_at AT TIME ZONE 'UTC';
ALTER TABLE workers ALTER COLUMN capacity_at TYPE TIMESTAMPTZ USING capacity_at AT TIME ZONE 'UTC';
ALTER TABLE build_live_logs ALTER COLUMN created_at TYPE TIMESTAMPTZ USING created_at AT TIME ZONE 'UTC';

-- +goose Down
ALTER TABLE build_live_logs ALTER COLUMN created_at TYPE TIMESTAMP USING created_at AT TIME ZONE 'UTC';
ALTER TABLE workers ALTER COLUMN capacity_at TYPE TIMESTAMP USING capacity_at AT TIME ZONE 'UTC';
ALTER TABLE controllers ALTER COLUMN capacity_at TYPE TIMESTAMP USING capacity_at AT TIME ZONE 'UTC';
DROP TABLE build_run_deletions;
