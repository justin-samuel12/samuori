-- +goose Up
CREATE TABLE build_artifacts (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    run_id     TEXT NOT NULL REFERENCES build_runs (id),
    attempt_id TEXT NOT NULL REFERENCES build_attempts (id),
    generation INTEGER NOT NULL,
    kind       TEXT NOT NULL,
    digest     TEXT NOT NULL,
    size       INTEGER NOT NULL,
    created_at TIMESTAMP NOT NULL,
    UNIQUE (attempt_id, kind, digest)
);
CREATE INDEX build_artifacts_digest ON build_artifacts (digest);

CREATE TABLE artifact_holds (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations (id),
    digest      TEXT NOT NULL,
    reason      TEXT NOT NULL,
    actor_id    TEXT NOT NULL,
    created_at  TIMESTAMP NOT NULL,
    released_at TIMESTAMP
);
CREATE INDEX artifact_holds_digest ON artifact_holds (digest);

-- +goose Down
DROP TABLE artifact_holds;
DROP TABLE build_artifacts;
