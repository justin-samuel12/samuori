-- +goose Up
-- Controllers record the name in their certificates and their API certificate pin; removed controller hosts are
-- remembered so their identity is refused afterwards (ADR 0045).
ALTER TABLE controllers ADD COLUMN host TEXT NOT NULL DEFAULT '';
ALTER TABLE controllers ADD COLUMN cert_pin TEXT NOT NULL DEFAULT '';

CREATE TABLE controller_removals (
    hostname     TEXT PRIMARY KEY,
    host         TEXT NOT NULL,
    cert_pins    TEXT NOT NULL DEFAULT '',
    org_id       TEXT NOT NULL,
    reason       TEXT NOT NULL,
    requested_by TEXT NOT NULL,
    requested_at TIMESTAMPTZ NOT NULL,
    state        TEXT NOT NULL CHECK (state IN ('requested', 'wiping', 'removed', 'rejoined')),
    outcome      TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('', 'wiped', 'not-confirmed')),
    detail       TEXT NOT NULL DEFAULT '',
    updated_at   TIMESTAMPTZ NOT NULL
);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON controller_removals TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE controller_removals;
ALTER TABLE controllers DROP COLUMN cert_pin;
ALTER TABLE controllers DROP COLUMN host;
