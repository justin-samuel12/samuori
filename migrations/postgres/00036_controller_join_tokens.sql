-- +goose Up
CREATE TABLE controller_join_tokens (
    id         TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    used_host  TEXT,
    revoked_at TIMESTAMPTZ
);

-- +goose Down
DROP TABLE controller_join_tokens;
