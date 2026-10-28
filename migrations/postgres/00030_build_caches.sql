-- +goose Up
CREATE TABLE cache_entries (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL REFERENCES organizations (id),
    project_id   TEXT NOT NULL,
    scope        TEXT NOT NULL,
    digest       TEXT NOT NULL,
    size         BIGINT NOT NULL,
    run_id       TEXT NOT NULL,
    attempt_id   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    UNIQUE (attempt_id, digest)
);
CREATE INDEX cache_entries_scope ON cache_entries (org_id, scope, created_at);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON cache_entries TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE cache_entries;
