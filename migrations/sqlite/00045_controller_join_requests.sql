-- +goose Up
-- Controller join requests wait for an administrator before the join bundle is sent (ADR 0045).
CREATE TABLE controller_join_requests (
    id          TEXT PRIMARY KEY,
    token_id    TEXT NOT NULL UNIQUE REFERENCES controller_join_tokens (id),
    host        TEXT NOT NULL,
    remote_addr TEXT NOT NULL,
    code        TEXT NOT NULL,
    secret_hash TEXT NOT NULL,
    state       TEXT NOT NULL CHECK (state IN ('pending', 'accepted', 'rejected', 'completed', 'expired')),
    decided_by  TEXT NOT NULL DEFAULT '',
    decided_at  TIMESTAMP,
    reason      TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMP NOT NULL,
    expires_at  TIMESTAMP NOT NULL
);

-- +goose Down
DROP TABLE controller_join_requests;
