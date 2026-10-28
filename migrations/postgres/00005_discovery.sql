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
    created_at    TIMESTAMPTZ NOT NULL,
    completed_at  TIMESTAMPTZ
);
CREATE INDEX discoveries_snapshot ON discoveries (snapshot_id);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE ON discoveries TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE discoveries;
