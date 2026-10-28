-- +goose Up
CREATE TABLE approvals (
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
CREATE INDEX approvals_state ON approvals (org_id, state);
CREATE UNIQUE INDEX approvals_subject ON approvals (subject_type, subject_id);

CREATE TABLE secret_binding_requests (
    id            TEXT PRIMARY KEY,
    org_id        TEXT NOT NULL REFERENCES organizations (id),
    project_id    TEXT NOT NULL REFERENCES projects (id),
    logical_id    TEXT NOT NULL,
    justification TEXT NOT NULL,
    requested_by  TEXT NOT NULL,
    created_at    TIMESTAMP NOT NULL
);

ALTER TABLE destinations ADD COLUMN requires_approval INTEGER NOT NULL DEFAULT 0;
ALTER TABLE publications ADD COLUMN approval_id TEXT;

-- +goose Down
ALTER TABLE publications DROP COLUMN approval_id;
ALTER TABLE destinations DROP COLUMN requires_approval;
DROP TABLE secret_binding_requests;
DROP TABLE approvals;
