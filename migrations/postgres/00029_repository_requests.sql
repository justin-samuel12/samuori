-- +goose Up
CREATE TABLE repository_requests (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL REFERENCES organizations (id),
    project_id     TEXT NOT NULL REFERENCES projects (id),
    registry_id    TEXT,
    repository     TEXT NOT NULL,
    justification  TEXT NOT NULL,
    requested_by   TEXT NOT NULL,
    destination_id TEXT,
    created_at     TIMESTAMPTZ NOT NULL
);
CREATE INDEX repository_requests_project ON repository_requests (project_id, created_at);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON repository_requests TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE repository_requests;
