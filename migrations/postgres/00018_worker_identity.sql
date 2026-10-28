-- +goose Up
ALTER TABLE workers ADD COLUMN prev_cert_serial TEXT NOT NULL DEFAULT '';
ALTER TABLE workers ADD COLUMN bundle_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workers ADD COLUMN revoked_at TIMESTAMPTZ;
ALTER TABLE workers ADD COLUMN revoke_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE workers ADD COLUMN compromised_since TIMESTAMPTZ;
ALTER TABLE workers ADD COLUMN replaced_by TEXT NOT NULL DEFAULT '';

CREATE TABLE artifact_quarantines (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL REFERENCES organizations (id),
    digest         TEXT NOT NULL,
    worker_id      TEXT NOT NULL,
    reason         TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    released_at    TIMESTAMPTZ,
    released_by    TEXT NOT NULL DEFAULT '',
    release_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX artifact_quarantines_digest ON artifact_quarantines (org_id, digest);

CREATE TABLE locations (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations (id),
    name        TEXT NOT NULL,
    description TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    UNIQUE (org_id, name)
);
INSERT INTO locations (id, org_id, name, description, created_at)
    SELECT 'loc-' || org_id || '-' || location, org_id, location, '', MIN(created_at) FROM worker_pools GROUP BY org_id, location;

CREATE TABLE project_locations (
    project_id TEXT NOT NULL REFERENCES projects (id),
    location   TEXT NOT NULL,
    PRIMARY KEY (project_id, location)
);

ALTER TABLE build_runs ADD COLUMN preferred_location TEXT NOT NULL DEFAULT '';

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE ON artifact_quarantines TO samuori_app;
        GRANT SELECT, INSERT, UPDATE, DELETE ON locations TO samuori_app;
        GRANT DELETE ON worker_pools TO samuori_app;
        GRANT SELECT, INSERT, DELETE ON project_locations TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
ALTER TABLE build_runs DROP COLUMN preferred_location;
DROP TABLE project_locations;
DROP TABLE locations;
DROP TABLE artifact_quarantines;
ALTER TABLE workers DROP COLUMN replaced_by;
ALTER TABLE workers DROP COLUMN compromised_since;
ALTER TABLE workers DROP COLUMN revoke_reason;
ALTER TABLE workers DROP COLUMN revoked_at;
ALTER TABLE workers DROP COLUMN bundle_version;
ALTER TABLE workers DROP COLUMN prev_cert_serial;
