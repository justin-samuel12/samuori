-- +goose Up
-- Kubernetes artifact generation (ADR 0048): a conversion, its provider runs (leased to workers with fencing) and
-- its versioned results.
CREATE TABLE kube_conversions (
    id               TEXT PRIMARY KEY,
    org_id           TEXT NOT NULL REFERENCES organizations (id),
    project_id       TEXT NOT NULL REFERENCES projects (id),
    name             TEXT NOT NULL,
    provider         TEXT NOT NULL,
    input            TEXT NOT NULL,
    options          TEXT NOT NULL,
    state            TEXT NOT NULL,
    current_revision INTEGER NOT NULL DEFAULT 0,
    created_by       TEXT NOT NULL,
    created_at       TIMESTAMP NOT NULL,
    updated_at       TIMESTAMP NOT NULL
);
CREATE INDEX kube_conversions_project ON kube_conversions (project_id, created_at);

CREATE TABLE kube_conversion_runs (
    id                 TEXT PRIMARY KEY,
    conversion_id      TEXT NOT NULL REFERENCES kube_conversions (id),
    org_id             TEXT NOT NULL REFERENCES organizations (id),
    number             INTEGER NOT NULL,
    options            TEXT NOT NULL,
    answers            TEXT NOT NULL,
    job                TEXT NOT NULL,
    job_digest         TEXT NOT NULL,
    inline_files       TEXT NOT NULL,
    context            TEXT NOT NULL,
    provider_version   TEXT NOT NULL,
    state              TEXT NOT NULL,
    generation         BIGINT NOT NULL DEFAULT 0,
    current_attempt_id TEXT,
    attempts_used      INTEGER NOT NULL DEFAULT 0,
    max_attempts       INTEGER NOT NULL,
    lease_worker_id    TEXT,
    lease_pool_id      TEXT,
    lease_deadline     TIMESTAMP,
    isolation_profile  TEXT,
    failure_class      TEXT,
    detail             TEXT,
    cleanup            TEXT,
    tool_digest        TEXT,
    questions          TEXT NOT NULL DEFAULT '[]',
    revision           INTEGER,
    operation_id       TEXT,
    created_by         TEXT NOT NULL,
    created_at         TIMESTAMP NOT NULL,
    updated_at         TIMESTAMP NOT NULL,
    ended_at           TIMESTAMP,
    UNIQUE (conversion_id, number)
);
CREATE INDEX kube_conversion_runs_state ON kube_conversion_runs (state);
CREATE INDEX kube_conversion_runs_worker ON kube_conversion_runs (lease_worker_id, state);
CREATE UNIQUE INDEX kube_conversion_runs_attempt ON kube_conversion_runs (current_attempt_id);

CREATE TABLE kube_conversion_revisions (
    conversion_id TEXT NOT NULL REFERENCES kube_conversions (id),
    number        INTEGER NOT NULL,
    run_id        TEXT REFERENCES kube_conversion_runs (id),
    parent        INTEGER,
    origin        TEXT NOT NULL,
    findings      TEXT NOT NULL,
    lineage       TEXT NOT NULL,
    errors        INTEGER NOT NULL,
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMP NOT NULL,
    PRIMARY KEY (conversion_id, number)
);

CREATE TABLE kube_conversion_files (
    conversion_id TEXT NOT NULL REFERENCES kube_conversions (id),
    revision      INTEGER NOT NULL,
    path          TEXT NOT NULL,
    digest        TEXT NOT NULL,
    size          BIGINT NOT NULL,
    content       TEXT NOT NULL,
    PRIMARY KEY (conversion_id, revision, path)
);

-- +goose Down
DROP TABLE kube_conversion_files;
DROP TABLE kube_conversion_revisions;
DROP TABLE kube_conversion_runs;
DROP TABLE kube_conversions;
