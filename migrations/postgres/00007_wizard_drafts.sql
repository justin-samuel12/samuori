-- +goose Up
CREATE TABLE wizard_drafts (
    user_id    TEXT NOT NULL REFERENCES users (id),
    project_id TEXT NOT NULL REFERENCES projects (id),
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    version    INTEGER NOT NULL,
    data       TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, project_id)
);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON wizard_drafts TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE wizard_drafts;
