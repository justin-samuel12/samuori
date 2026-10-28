-- +goose Up
-- The administrator's target topology and declared resilience (ADR 0046).
CREATE TABLE topology_plans (
    org_id              TEXT PRIMARY KEY REFERENCES organizations (id),
    deployment          TEXT NOT NULL CHECK (deployment IN ('single-server', 'cluster')),
    controllers         INTEGER NOT NULL CHECK (controllers BETWEEN 1 AND 7),
    workers             INTEGER NOT NULL CHECK (workers BETWEEN 0 AND 1000),
    database_replicated INTEGER NOT NULL DEFAULT 0,
    storage_replicated  INTEGER NOT NULL DEFAULT 0,
    updated_by          TEXT NOT NULL,
    updated_at          TIMESTAMP NOT NULL
);

-- +goose Down
DROP TABLE topology_plans;
