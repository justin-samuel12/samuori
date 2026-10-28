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
    created_at         TIMESTAMPTZ NOT NULL,
    closed_at          TIMESTAMPTZ
);
CREATE INDEX base_image_requests_project ON base_image_requests (project_id, created_at);
CREATE UNIQUE INDEX base_image_requests_pending ON base_image_requests (org_id, pending_repository);

ALTER TABLE approvals DROP CONSTRAINT approvals_subject_type_check;
ALTER TABLE approvals ADD CONSTRAINT approvals_subject_type_check
    CHECK (subject_type IN ('publication', 'security-exception', 'secret-binding', 'repository-request', 'base-image-request'));

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE ON base_image_requests TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DELETE FROM approvals WHERE subject_type = 'base-image-request';
ALTER TABLE approvals DROP CONSTRAINT approvals_subject_type_check;
ALTER TABLE approvals ADD CONSTRAINT approvals_subject_type_check
    CHECK (subject_type IN ('publication', 'security-exception', 'secret-binding', 'repository-request'));
DROP TABLE base_image_requests;
