-- +goose Up
CREATE TABLE controller_join_tokens (
    id         TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    created_by TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    expires_at TIMESTAMP NOT NULL,
    used_at    TIMESTAMP,
    used_host  TEXT,
    revoked_at TIMESTAMP
);

-- +goose Down
DROP TABLE controller_join_tokens;
