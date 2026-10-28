-- +goose Up
CREATE TABLE build_specs (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL REFERENCES organizations (id),
    project_id      TEXT NOT NULL REFERENCES projects (id),
    name            TEXT NOT NULL,
    latest_revision INTEGER NOT NULL,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    UNIQUE (project_id, name)
);

CREATE TABLE spec_revisions (
    spec_id     TEXT NOT NULL REFERENCES build_specs (id),
    revision    INTEGER NOT NULL,
    digest      TEXT NOT NULL,
    api_version TEXT NOT NULL,
    document    TEXT NOT NULL,
    editor_mode TEXT,
    note        TEXT,
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (spec_id, revision)
);

CREATE TABLE recipe_status (
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    recipe_id  TEXT NOT NULL,
    version    TEXT NOT NULL,
    status     TEXT NOT NULL,
    reason     TEXT,
    updated_by TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (org_id, recipe_id, version)
);

CREATE TABLE policies (
    id      TEXT PRIMARY KEY,
    org_id  TEXT NOT NULL REFERENCES organizations (id),
    kind    TEXT NOT NULL,
    version INTEGER NOT NULL,
    UNIQUE (org_id, kind)
);

CREATE TABLE policy_versions (
    policy_id  TEXT NOT NULL REFERENCES policies (id),
    version    INTEGER NOT NULL,
    document   TEXT NOT NULL,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (policy_id, version)
);

-- +goose StatementBegin
CREATE FUNCTION samuori_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% rows are immutable', TG_TABLE_NAME;
END;
$$;
CREATE TRIGGER spec_revisions_immutable BEFORE UPDATE OR DELETE ON spec_revisions
    FOR EACH ROW EXECUTE FUNCTION samuori_immutable();
CREATE TRIGGER policy_versions_immutable BEFORE UPDATE OR DELETE ON policy_versions
    FOR EACH ROW EXECUTE FUNCTION samuori_immutable();
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE ON build_specs, recipe_status, policies TO samuori_app;
        GRANT SELECT, INSERT ON spec_revisions, policy_versions TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER policy_versions_immutable ON policy_versions;
DROP TRIGGER spec_revisions_immutable ON spec_revisions;
DROP FUNCTION samuori_immutable();
DROP TABLE policy_versions;
DROP TABLE policies;
DROP TABLE recipe_status;
DROP TABLE spec_revisions;
DROP TABLE build_specs;
