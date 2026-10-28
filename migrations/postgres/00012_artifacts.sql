-- +goose Up
CREATE TABLE build_artifacts (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    run_id     TEXT NOT NULL REFERENCES build_runs (id),
    attempt_id TEXT NOT NULL REFERENCES build_attempts (id),
    generation BIGINT NOT NULL,
    kind       TEXT NOT NULL,
    digest     TEXT NOT NULL,
    size       BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (attempt_id, kind, digest)
);
CREATE INDEX build_artifacts_digest ON build_artifacts (digest);

CREATE TABLE artifact_holds (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations (id),
    digest      TEXT NOT NULL,
    reason      TEXT NOT NULL,
    actor_id    TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    released_at TIMESTAMPTZ
);
CREATE INDEX artifact_holds_digest ON artifact_holds (digest);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, DELETE ON build_artifacts TO samuori_app;
        GRANT SELECT, INSERT, UPDATE ON artifact_holds TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE artifact_holds;
DROP TABLE build_artifacts;
