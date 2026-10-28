-- +goose Up
-- Project members ask organization administrators to open egress presets or destinations for one project (ADR 0052).
-- evidence is built by the server from stored data. pending_key is cleared when the request is decided or expires,
-- so one request for the same method and entries stays open per project.
CREATE TABLE egress_access_requests (
    id            TEXT PRIMARY KEY,
    org_id        TEXT NOT NULL REFERENCES organizations (id),
    project_id    TEXT NOT NULL REFERENCES projects (id),
    method        TEXT NOT NULL CHECK (method IN ('dockerfile', 'recipe', 'railpack', 'buildpacks')),
    entries       TEXT NOT NULL,
    pending_key   TEXT,
    justification TEXT NOT NULL,
    evidence      TEXT NOT NULL,
    requested_by  TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    closed_at     TIMESTAMPTZ
);
CREATE INDEX egress_access_requests_project ON egress_access_requests (project_id, created_at);
CREATE UNIQUE INDEX egress_access_requests_pending ON egress_access_requests (project_id, pending_key);

CREATE TABLE project_egress_allowances (
    id            TEXT PRIMARY KEY,
    org_id        TEXT NOT NULL REFERENCES organizations (id),
    project_id    TEXT NOT NULL REFERENCES projects (id),
    kind          TEXT NOT NULL CHECK (kind IN ('preset', 'destination')),
    preset        TEXT,
    host          TEXT,
    port          INTEGER,
    methods       TEXT NOT NULL,
    reason        TEXT NOT NULL,
    request_id    TEXT NOT NULL REFERENCES egress_access_requests (id),
    status        TEXT NOT NULL CHECK (status IN ('active', 'revoked')),
    requested_by  TEXT NOT NULL,
    approved_by   TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    expires_at    TIMESTAMPTZ,
    revoked_by    TEXT,
    revoked_at    TIMESTAMPTZ,
    revoke_reason TEXT,
    CHECK ((kind = 'preset' AND preset IS NOT NULL AND host IS NULL) OR (kind = 'destination' AND preset IS NULL AND host IS NOT NULL AND port BETWEEN 1 AND 65535))
);
CREATE INDEX project_egress_allowances_project ON project_egress_allowances (project_id, status);

ALTER TABLE approvals DROP CONSTRAINT approvals_subject_type_check;
ALTER TABLE approvals ADD CONSTRAINT approvals_subject_type_check
    CHECK (subject_type IN ('publication', 'security-exception', 'secret-binding', 'repository-request', 'base-image-request', 'egress-access-request'));

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE ON egress_access_requests, project_egress_allowances TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DELETE FROM approvals WHERE subject_type = 'egress-access-request';
ALTER TABLE approvals DROP CONSTRAINT approvals_subject_type_check;
ALTER TABLE approvals ADD CONSTRAINT approvals_subject_type_check
    CHECK (subject_type IN ('publication', 'security-exception', 'secret-binding', 'repository-request', 'base-image-request'));
DROP TABLE project_egress_allowances;
DROP TABLE egress_access_requests;
