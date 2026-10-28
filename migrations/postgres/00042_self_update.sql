-- +goose Up
-- Signed releases administrators uploaded, and rollouts that update controllers, then workers (ADR 0044).
CREATE TABLE releases (
    version     TEXT NOT NULL,
    arch        TEXT NOT NULL,
    digest      TEXT NOT NULL,
    size        BIGINT NOT NULL,
    checksums   BYTEA NOT NULL,
    signature   BYTEA NOT NULL,
    uploaded_by TEXT NOT NULL,
    uploaded_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (version, arch)
);

CREATE TABLE rollouts (
    id          TEXT PRIMARY KEY,
    version     TEXT NOT NULL,
    state       TEXT NOT NULL CHECK (state IN ('running', 'succeeded', 'failed', 'cancelled')),
    reason      TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ
);

CREATE TABLE rollout_steps (
    rollout_id TEXT NOT NULL REFERENCES rollouts (id),
    seq        INTEGER NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('controller', 'worker')),
    target     TEXT NOT NULL,
    name       TEXT NOT NULL,
    state      TEXT NOT NULL CHECK (state IN ('pending', 'draining', 'requested', 'applying', 'testing', 'succeeded', 'failed', 'skipped')),
    detail     TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (rollout_id, seq)
);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON releases, rollouts, rollout_steps TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE rollout_steps;
DROP TABLE rollouts;
DROP TABLE releases;
