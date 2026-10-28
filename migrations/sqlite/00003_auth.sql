-- +goose Up
CREATE TABLE user_credentials (
    user_id       TEXT PRIMARY KEY REFERENCES users (id),
    password_hash TEXT NOT NULL,
    updated_at    TIMESTAMP NOT NULL
);

CREATE TABLE sessions (
    token_hash   TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id),
    csrf_hash    TEXT NOT NULL,
    created_at   TIMESTAMP NOT NULL,
    last_seen_at TIMESTAMP NOT NULL,
    revoked_at   TIMESTAMP
);
CREATE INDEX sessions_user ON sessions (user_id);

CREATE TABLE api_tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id),
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    scopes       TEXT NOT NULL,
    expires_at   TIMESTAMP NOT NULL,
    created_at   TIMESTAMP NOT NULL,
    last_used_at TIMESTAMP,
    revoked_at   TIMESTAMP
);
CREATE INDEX api_tokens_user ON api_tokens (user_id);

CREATE TABLE bootstrap_tokens (
    token_hash TEXT PRIMARY KEY,
    expires_at TIMESTAMP NOT NULL,
    used_at    TIMESTAMP
);

-- +goose Down
DROP TABLE bootstrap_tokens;
DROP TABLE api_tokens;
DROP TABLE sessions;
DROP TABLE user_credentials;
