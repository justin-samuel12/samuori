-- +goose Up
-- Workers being removed: drained, then their host wiped, then revoked (ADR 0045).
CREATE TABLE worker_removals (
    worker_id    TEXT PRIMARY KEY REFERENCES workers (id),
    org_id       TEXT NOT NULL,
    reason       TEXT NOT NULL,
    requested_by TEXT NOT NULL,
    requested_at TIMESTAMPTZ NOT NULL,
    state        TEXT NOT NULL CHECK (state IN ('draining', 'wiping', 'removed')),
    outcome      TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('', 'wiped', 'not-confirmed')),
    detail       TEXT NOT NULL DEFAULT '',
    updated_at   TIMESTAMPTZ NOT NULL
);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON worker_removals TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE worker_removals;
