-- +goose Up
-- A requester may withdraw a pending request; a deleted pipeline keeps its row and run history (ADR 0062).
CREATE TABLE approvals_new (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL REFERENCES organizations (id),
    project_id   TEXT NOT NULL REFERENCES projects (id),
    subject_type TEXT NOT NULL CHECK (subject_type IN ('publication', 'security-exception', 'secret-binding', 'repository-request', 'base-image-request', 'egress-access-request')),
    subject_id   TEXT NOT NULL,
    state        TEXT NOT NULL CHECK (state IN ('pending', 'approved', 'rejected', 'expired', 'withdrawn')),
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

ALTER TABLE pipelines ADD COLUMN deleted_by TEXT;
ALTER TABLE pipelines ADD COLUMN deleted_at TIMESTAMP;

-- +goose Down
ALTER TABLE pipelines DROP COLUMN deleted_at;
ALTER TABLE pipelines DROP COLUMN deleted_by;
UPDATE approvals SET state = 'expired' WHERE state = 'withdrawn';
CREATE TABLE approvals_old (
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
INSERT INTO approvals_old SELECT id, org_id, project_id, subject_type, subject_id, state, summary, requested_by, decided_by, reason, created_at, expires_at, decided_at, valid_until FROM approvals;
DROP TABLE approvals;
ALTER TABLE approvals_old RENAME TO approvals;
CREATE INDEX approvals_state ON approvals (org_id, state);
CREATE UNIQUE INDEX approvals_subject ON approvals (subject_type, subject_id);
