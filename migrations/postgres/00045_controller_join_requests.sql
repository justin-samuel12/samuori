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
    decided_at  TIMESTAMPTZ,
    reason      TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL
);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON controller_join_requests TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE controller_join_requests;
