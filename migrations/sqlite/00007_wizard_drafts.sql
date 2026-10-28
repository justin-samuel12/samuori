-- +goose Up
CREATE TABLE wizard_drafts (
    user_id    TEXT NOT NULL REFERENCES users (id),
    project_id TEXT NOT NULL REFERENCES projects (id),
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    version    INTEGER NOT NULL,
    data       TEXT NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    PRIMARY KEY (user_id, project_id)
);

-- +goose Down
DROP TABLE wizard_drafts;
