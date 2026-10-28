-- +goose Up
-- Runtime versions administrators add to built-in recipes (ADR 0051). Shipped runtimes stay in the catalog.
CREATE TABLE organization_runtimes (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL REFERENCES organizations (id),
    recipe_id      TEXT NOT NULL,
    recipe_version TEXT NOT NULL,
    label          TEXT NOT NULL,
    build_image    TEXT NOT NULL,
    run_image      TEXT NOT NULL,
    note           TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL CHECK (status IN ('draft', 'approved', 'withdrawn')),
    reason         TEXT NOT NULL DEFAULT '',
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    approved_by    TEXT,
    approved_at    TIMESTAMPTZ,
    withdrawn_by   TEXT,
    withdrawn_at   TIMESTAMPTZ
);
CREATE UNIQUE INDEX organization_runtimes_active ON organization_runtimes (org_id, recipe_id, recipe_version, label) WHERE status <> 'withdrawn';

-- +goose Down
DROP TABLE organization_runtimes;
