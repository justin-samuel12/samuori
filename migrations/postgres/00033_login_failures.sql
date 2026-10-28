-- +goose Up
CREATE TABLE login_failures (
    account   TEXT NOT NULL,
    failed_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX login_failures_account ON login_failures (account, failed_at);

CREATE TABLE login_lockouts (
    account      TEXT PRIMARY KEY,
    locked_until TIMESTAMPTZ NOT NULL
);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON login_failures, login_lockouts TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE login_lockouts;
DROP TABLE login_failures;
