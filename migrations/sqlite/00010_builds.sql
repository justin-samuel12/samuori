-- +goose Up
CREATE TABLE build_plans (
    digest     TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    plan       TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL
);

CREATE TABLE build_runs (
    id                 TEXT PRIMARY KEY,
    org_id             TEXT NOT NULL REFERENCES organizations (id),
    project_id         TEXT NOT NULL REFERENCES projects (id),
    team_id            TEXT NOT NULL REFERENCES teams (id),
    spec_id            TEXT NOT NULL REFERENCES build_specs (id),
    spec_revision      INTEGER NOT NULL,
    spec_digest        TEXT NOT NULL,
    snapshot_id        TEXT NOT NULL REFERENCES source_snapshots (id),
    snapshot_digest    TEXT NOT NULL,
    plan_digest        TEXT REFERENCES build_plans (digest),
    state              TEXT NOT NULL,
    failure_class      TEXT,
    blocked_reason     TEXT,
    platforms          TEXT NOT NULL,
    isolation_profile  TEXT NOT NULL,
    secret_bearing     INTEGER NOT NULL,
    untrusted          INTEGER NOT NULL,
    priority           INTEGER NOT NULL,
    generation         INTEGER NOT NULL,
    current_attempt_id TEXT,
    lease_worker_id    TEXT,
    lease_deadline     TIMESTAMP,
    attempts_used      INTEGER NOT NULL,
    max_attempts       INTEGER NOT NULL,
    queue_deadline     TIMESTAMP NOT NULL,
    artifact_digest    TEXT,
    requested_by       TEXT NOT NULL,
    idempotency_key    TEXT NOT NULL,
    created_at         TIMESTAMP NOT NULL,
    updated_at         TIMESTAMP NOT NULL,
    UNIQUE (requested_by, idempotency_key)
);
CREATE INDEX build_runs_queue ON build_runs (state, priority, created_at);
CREATE INDEX build_runs_project ON build_runs (project_id, id);

CREATE TABLE build_attempts (
    id              TEXT PRIMARY KEY,
    run_id          TEXT NOT NULL REFERENCES build_runs (id),
    number          INTEGER NOT NULL,
    generation      INTEGER NOT NULL,
    worker_id       TEXT NOT NULL,
    pool_id         TEXT NOT NULL,
    state           TEXT NOT NULL,
    lease_deadline  TIMESTAMP NOT NULL,
    failure_class   TEXT,
    detail          TEXT,
    artifact_digest TEXT,
    cleanup         TEXT NOT NULL,
    started_at      TIMESTAMP NOT NULL,
    ended_at        TIMESTAMP,
    UNIQUE (run_id, number),
    UNIQUE (run_id, generation)
);

CREATE TABLE build_events (
    run_id TEXT NOT NULL REFERENCES build_runs (id),
    seq    INTEGER NOT NULL,
    at     TIMESTAMP NOT NULL,
    kind   TEXT NOT NULL,
    data   TEXT NOT NULL,
    PRIMARY KEY (run_id, seq)
);

-- +goose StatementBegin
CREATE TRIGGER build_plans_immutable BEFORE UPDATE ON build_plans
BEGIN
    SELECT RAISE(ABORT, 'build plans are immutable');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER build_plans_immutable;
DROP TABLE build_events;
DROP TABLE build_attempts;
DROP TABLE build_runs;
DROP TABLE build_plans;
