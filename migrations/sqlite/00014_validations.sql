-- +goose Up
CREATE TABLE validation_runs (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL REFERENCES organizations (id),
    project_id      TEXT NOT NULL REFERENCES projects (id),
    artifact_id     TEXT NOT NULL REFERENCES build_artifacts (id),
    artifact_digest TEXT NOT NULL,
    build_run_id    TEXT REFERENCES build_runs (id),
    source_run_id   TEXT REFERENCES validation_runs (id),
    trigger_kind    TEXT NOT NULL CHECK (trigger_kind IN ('build', 'rescan', 'promotion')),
    state           TEXT NOT NULL CHECK (state IN ('pending', 'running', 'completed', 'cancelled', 'timed_out', 'errored')),
    verdict         TEXT CHECK (verdict IN ('passed', 'failed', 'blocked', 'incomplete')),
    reason          TEXT,
    plan            TEXT NOT NULL,
    policy_versions TEXT NOT NULL,
    requested_by    TEXT,
    idempotency_key TEXT,
    created_at      TIMESTAMP NOT NULL,
    started_at      TIMESTAMP,
    finished_at     TIMESTAMP
);
CREATE INDEX validation_runs_artifact ON validation_runs (artifact_id, created_at);
CREATE INDEX validation_runs_state ON validation_runs (state);
CREATE UNIQUE INDEX validation_runs_key ON validation_runs (requested_by, idempotency_key) WHERE idempotency_key IS NOT NULL;

CREATE TABLE check_results (
    id                TEXT PRIMARY KEY,
    validation_run_id TEXT NOT NULL REFERENCES validation_runs (id),
    check_id          TEXT NOT NULL,
    category          TEXT NOT NULL,
    origin            TEXT NOT NULL CHECK (origin IN ('worker', 'controller')),
    required          INTEGER NOT NULL,
    outcome           TEXT NOT NULL CHECK (outcome IN ('passed', 'failed', 'blocked', 'skipped', 'errored')),
    reason            TEXT NOT NULL,
    tool              TEXT NOT NULL,
    findings          TEXT NOT NULL,
    output_digest     TEXT,
    created_at        TIMESTAMP NOT NULL,
    UNIQUE (validation_run_id, check_id)
);
CREATE INDEX check_results_output ON check_results (output_digest);

-- +goose Down
DROP TABLE check_results;
DROP TABLE validation_runs;
