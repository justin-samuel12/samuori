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
    created_at    TIMESTAMP NOT NULL,
    closed_at     TIMESTAMP
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
    created_at    TIMESTAMP NOT NULL,
    expires_at    TIMESTAMP,
    revoked_by    TEXT,
    revoked_at    TIMESTAMP,
    revoke_reason TEXT,
    CHECK ((kind = 'preset' AND preset IS NOT NULL AND host IS NULL) OR (kind = 'destination' AND preset IS NULL AND host IS NOT NULL AND port BETWEEN 1 AND 65535))
);
CREATE INDEX project_egress_allowances_project ON project_egress_allowances (project_id, status);

CREATE TABLE approvals_new (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL REFERENCES organizations (id),
    project_id   TEXT NOT NULL REFERENCES projects (id),
    subject_type TEXT NOT NULL CHECK (subject_type IN ('publication', 'security-exception', 'secret-binding', 'repository-request', 'base-image-request', 'egress-access-request')),
    subject_id   TEXT NOT NULL,
    state        TEXT NOT NULL CHECK (state IN ('pending', 'approved', 'rejected', 'expired')),
    summary      TEXT NOT NULL,
    requested_by TEXT NOT NULL,
    decided_by   TEXT,
    reason       TEXT,
    created_at   TIMESTAMP NOT NULL,
    expires_at   TIMESTAMP NOT NULL,
    decided_at   TIMESTAMP,
    valid_until  TIMESTAMP
);
INSERT INTO approvals_new SELECT id, org_id, project_id, subject_type, subject_id, state, summary, requested_by, decided_by, reason, created_at, expires_at, decided_at, valid_until FROM approvals;
DROP TABLE approvals;
ALTER TABLE approvals_new RENAME TO approvals;
CREATE INDEX approvals_state ON approvals (org_id, state);
CREATE UNIQUE INDEX approvals_subject ON approvals (subject_type, subject_id);

-- +goose Down
DELETE FROM approvals WHERE subject_type = 'egress-access-request';
CREATE TABLE approvals_old (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL REFERENCES organizations (id),
    project_id   TEXT NOT NULL REFERENCES projects (id),
    subject_type TEXT NOT NULL CHECK (subject_type IN ('publication', 'security-exception', 'secret-binding', 'repository-request', 'base-image-request')),
    subject_id   TEXT NOT NULL,
    state        TEXT NOT NULL CHECK (state IN ('pending', 'approved', 'rejected', 'expired')),
    summary      TEXT NOT NULL,
    requested_by TEXT NOT NULL,
    decided_by   TEXT,
    reason       TEXT,
    created_at   TIMESTAMP NOT NULL,
    expires_at   TIMESTAMP NOT NULL,
    decided_at   TIMESTAMP,
    valid_until  TIMESTAMP
);
INSERT INTO approvals_old SELECT id, org_id, project_id, subject_type, subject_id, state, summary, requested_by, decided_by, reason, created_at, expires_at, decided_at, valid_until FROM approvals;
DROP TABLE approvals;
ALTER TABLE approvals_old RENAME TO approvals;
CREATE INDEX approvals_state ON approvals (org_id, state);
CREATE UNIQUE INDEX approvals_subject ON approvals (subject_type, subject_id);
DROP TABLE project_egress_allowances;
DROP TABLE egress_access_requests;
