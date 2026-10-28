-- +goose Up
CREATE TABLE login_failures (
    account   TEXT NOT NULL,
    failed_at TIMESTAMP NOT NULL
);
CREATE INDEX login_failures_account ON login_failures (account, failed_at);

CREATE TABLE login_lockouts (
    account      TEXT PRIMARY KEY,
    locked_until TIMESTAMP NOT NULL
);

-- +goose Down
DROP TABLE login_lockouts;
DROP TABLE login_failures;
