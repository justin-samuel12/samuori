-- +goose Up
CREATE TABLE federation_peers (
    id            TEXT PRIMARY KEY,
    org_id        TEXT NOT NULL REFERENCES organizations (id),
    name          TEXT NOT NULL,
    endpoint      TEXT NOT NULL,
    ca_pin        TEXT NOT NULL,
    remote_org_id TEXT NOT NULL,
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    disabled_at   TIMESTAMPTZ,
    UNIQUE (org_id, name)
);

CREATE TABLE federation_grants (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    peer_id    TEXT NOT NULL REFERENCES federation_peers (id),
    project_id TEXT NOT NULL REFERENCES projects (id),
    locations  TEXT NOT NULL,
    max_active INTEGER NOT NULL,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ
);

CREATE TABLE federation_routes (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL REFERENCES organizations (id),
    peer_id         TEXT NOT NULL REFERENCES federation_peers (id),
    location_id     TEXT NOT NULL REFERENCES locations (id),
    remote_location TEXT NOT NULL,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    UNIQUE (location_id)
);

CREATE TABLE federation_delegations (
    id                TEXT PRIMARY KEY,
    org_id            TEXT NOT NULL REFERENCES organizations (id),
    run_id            TEXT NOT NULL REFERENCES build_runs (id),
    peer_id           TEXT NOT NULL REFERENCES federation_peers (id),
    remote_location   TEXT NOT NULL,
    attempt_id        TEXT,
    state             TEXT NOT NULL,
    remote_run_id     TEXT,
    remote_state      TEXT,
    detail            TEXT,
    artifact_digest   TEXT,
    last_contact_at   TIMESTAMPTZ,
    unreachable_since TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL,
    UNIQUE (run_id)
);
CREATE INDEX federation_delegations_state ON federation_delegations (state, updated_at);

CREATE TABLE federation_inbound (
    peer_id       TEXT NOT NULL REFERENCES federation_peers (id),
    delegation_id TEXT NOT NULL,
    org_id        TEXT NOT NULL REFERENCES organizations (id),
    grant_id      TEXT NOT NULL REFERENCES federation_grants (id),
    run_id        TEXT REFERENCES build_runs (id),
    created_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (peer_id, delegation_id)
);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON federation_peers, federation_grants, federation_routes, federation_delegations, federation_inbound TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE federation_inbound;
DROP TABLE federation_delegations;
DROP TABLE federation_routes;
DROP TABLE federation_grants;
DROP TABLE federation_peers;
