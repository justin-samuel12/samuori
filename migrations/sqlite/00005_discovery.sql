-- +goose Up
CREATE TABLE discoveries (
    id            TEXT PRIMARY KEY,
    org_id        TEXT NOT NULL REFERENCES organizations (id),
    project_id    TEXT NOT NULL REFERENCES projects (id),
    snapshot_id   TEXT NOT NULL REFERENCES source_snapshots (id),
    status        TEXT NOT NULL,
    result        TEXT,
    sandbox       TEXT,
    error_code    TEXT,
    error_message TEXT,
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMP NOT NULL,
    completed_at  TIMESTAMP
);
CREATE INDEX discoveries_snapshot ON discoveries (snapshot_id);

-- +goose Down
DROP TABLE discoveries;
