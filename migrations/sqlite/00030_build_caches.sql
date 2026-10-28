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
    created_at   TIMESTAMP NOT NULL,
    last_used_at TIMESTAMP,
    UNIQUE (attempt_id, digest)
);
CREATE INDEX cache_entries_scope ON cache_entries (org_id, scope, created_at);

-- +goose Down
DROP TABLE cache_entries;
