-- +goose Up
-- A successful build's version, <UTC date it succeeded>.<n>, counted per project and day (ADR 0054).
ALTER TABLE build_runs ADD COLUMN version TEXT;
CREATE UNIQUE INDEX build_runs_version ON build_runs (project_id, version) WHERE version IS NOT NULL;
CREATE TABLE build_version_counters (
    project_id TEXT NOT NULL REFERENCES projects (id),
    day        TEXT NOT NULL,
    last       INTEGER NOT NULL,
    PRIMARY KEY (project_id, day)
);

-- +goose Down
DROP TABLE build_version_counters;
DROP INDEX build_runs_version;
ALTER TABLE build_runs DROP COLUMN version;
