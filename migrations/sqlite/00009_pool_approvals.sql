-- +goose Up
CREATE TABLE pool_approvals (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    pool_id    TEXT NOT NULL REFERENCES worker_pools (id),
    approved   INTEGER NOT NULL,
    reason     TEXT NOT NULL,
    actor_id   TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL
);
CREATE INDEX pool_approvals_pool ON pool_approvals (pool_id, created_at);

-- +goose StatementBegin
CREATE TRIGGER pool_approvals_immutable BEFORE UPDATE ON pool_approvals
BEGIN
    SELECT RAISE(ABORT, 'pool approvals are immutable');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER pool_approvals_immutable;
DROP TABLE pool_approvals;
