-- +goose Up
-- Project members ask organization administrators to add a base-image repository to the policy.
-- pending_repository is cleared when the request is decided or expires, so one request per repository stays open.
CREATE TABLE base_image_requests (
    id                 TEXT PRIMARY KEY,
    org_id             TEXT NOT NULL REFERENCES organizations (id),
    project_id         TEXT NOT NULL REFERENCES projects (id),
    repository         TEXT NOT NULL,
    pending_repository TEXT,
    justification      TEXT NOT NULL,
    requested_by       TEXT NOT NULL,
    policy_version     INTEGER,
    created_at         TIMESTAMP NOT NULL,
    closed_at          TIMESTAMP
);
CREATE INDEX base_image_requests_project ON base_image_requests (project_id, created_at);
CREATE UNIQUE INDEX base_image_requests_pending ON base_image_requests (org_id, pending_repository);

CREATE TABLE approvals_new (
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
INSERT INTO approvals_new SELECT id, org_id, project_id, subject_type, subject_id, state, summary, requested_by, decided_by, reason, created_at, expires_at, decided_at, valid_until FROM approvals;
DROP TABLE approvals;
ALTER TABLE approvals_new RENAME TO approvals;
CREATE INDEX approvals_state ON approvals (org_id, state);
CREATE UNIQUE INDEX approvals_subject ON approvals (subject_type, subject_id);

-- +goose Down
DELETE FROM approvals WHERE subject_type = 'base-image-request';
CREATE TABLE approvals_old (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL REFERENCES organizations (id),
    project_id   TEXT NOT NULL REFERENCES projects (id),
    subject_type TEXT NOT NULL CHECK (subject_type IN ('publication', 'security-exception', 'secret-binding', 'repository-request')),
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
DROP TABLE base_image_requests;
