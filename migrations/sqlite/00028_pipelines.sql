-- +goose Up
CREATE TABLE pipelines (
    id                    TEXT PRIMARY KEY,
    org_id                TEXT NOT NULL REFERENCES organizations (id),
    project_id            TEXT NOT NULL REFERENCES projects (id),
    name                  TEXT NOT NULL,
    spec_id               TEXT NOT NULL REFERENCES build_specs (id),
    spec_revision         INTEGER NOT NULL DEFAULT 0,
    repo_url              TEXT NOT NULL,
    refs                  TEXT NOT NULL,
    username              TEXT,
    token_secret_id       TEXT,
    allow_private_network INTEGER NOT NULL DEFAULT 0,
    hook_scheme           TEXT NOT NULL CHECK (hook_scheme IN ('hmac-sha256', 'gitlab-token')),
    hook_secret_id        TEXT NOT NULL,
    owner_id              TEXT NOT NULL,
    enabled               INTEGER NOT NULL DEFAULT 1,
    created_by            TEXT NOT NULL,
    created_at            TIMESTAMP NOT NULL,
    updated_at            TIMESTAMP NOT NULL,
    UNIQUE (project_id, name)
);
CREATE INDEX pipelines_org ON pipelines (org_id);

CREATE TABLE pipeline_runs (
    id               TEXT PRIMARY KEY,
    org_id           TEXT NOT NULL REFERENCES organizations (id),
    pipeline_id      TEXT NOT NULL REFERENCES pipelines (id),
    project_id       TEXT NOT NULL,
    trigger          TEXT NOT NULL CHECK (trigger IN ('webhook', 'manual', 'api')),
    ref              TEXT NOT NULL,
    requested_commit TEXT,
    commit_sha       TEXT,
    delivery_id      TEXT,
    state            TEXT NOT NULL CHECK (state IN ('queued', 'fetching', 'started', 'failed', 'skipped')),
    failure_code     TEXT,
    failure_message  TEXT,
    skipped_entries  INTEGER NOT NULL DEFAULT 0,
    snapshot_id      TEXT,
    build_run_id     TEXT,
    requested_by     TEXT NOT NULL,
    claimed_until    TIMESTAMP,
    created_at       TIMESTAMP NOT NULL,
    updated_at       TIMESTAMP NOT NULL,
    UNIQUE (pipeline_id, delivery_id)
);
CREATE INDEX pipeline_runs_pipeline ON pipeline_runs (pipeline_id, created_at);
CREATE INDEX pipeline_runs_due ON pipeline_runs (state, claimed_until);

-- +goose Down
DROP TABLE pipeline_runs;
DROP TABLE pipelines;
