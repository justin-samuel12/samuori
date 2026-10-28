-- +goose Up
-- A deleted build keeps its run, attempts, validations and audit trail; garbage collection stops keeping its image,
-- logs and cache (ADR 0053).
CREATE TABLE build_run_deletions (
    run_id     TEXT PRIMARY KEY REFERENCES build_runs (id),
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    deleted_by TEXT NOT NULL,
    reason     TEXT NOT NULL,
    deleted_at TIMESTAMP NOT NULL
);

-- +goose Down
DROP TABLE build_run_deletions;
