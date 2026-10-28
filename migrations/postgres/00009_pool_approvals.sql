-- +goose Up
CREATE TABLE pool_approvals (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    pool_id    TEXT NOT NULL REFERENCES worker_pools (id),
    approved   BOOLEAN NOT NULL,
    reason     TEXT NOT NULL,
    actor_id   TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX pool_approvals_pool ON pool_approvals (pool_id, created_at);

-- +goose StatementBegin
CREATE TRIGGER pool_approvals_immutable BEFORE UPDATE OR DELETE ON pool_approvals
    FOR EACH ROW EXECUTE FUNCTION samuori_immutable();
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT ON pool_approvals TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER pool_approvals_immutable ON pool_approvals;
DROP TABLE pool_approvals;
