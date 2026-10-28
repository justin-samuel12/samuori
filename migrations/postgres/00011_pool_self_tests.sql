-- +goose Up
CREATE TABLE pool_self_tests (
    id            TEXT PRIMARY KEY,
    org_id        TEXT NOT NULL REFERENCES organizations (id),
    pool_id       TEXT NOT NULL REFERENCES worker_pools (id),
    worker_id     TEXT NOT NULL,
    status        TEXT NOT NULL,
    failed_checks TEXT NOT NULL,
    started_at    TIMESTAMPTZ NOT NULL,
    finished_at   TIMESTAMPTZ
);
CREATE INDEX pool_self_tests_pool ON pool_self_tests (pool_id, started_at);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE ON pool_self_tests TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE pool_self_tests;
