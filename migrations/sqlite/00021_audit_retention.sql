-- +goose Up
CREATE TABLE audit_policies (
    org_id            TEXT PRIMARY KEY REFERENCES organizations (id),
    retention_days    INTEGER NOT NULL DEFAULT 0,
    export_enabled    INTEGER NOT NULL DEFAULT 0,
    require_export    INTEGER NOT NULL DEFAULT 1,
    exported_through  TIMESTAMP,
    last_export_at    TIMESTAMP,
    last_export_error TEXT,
    last_purge_at     TIMESTAMP,
    last_purge_count  INTEGER NOT NULL DEFAULT 0,
    updated_by        TEXT NOT NULL,
    updated_at        TIMESTAMP NOT NULL
);

CREATE TABLE audit_purges (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations (id),
    cutoff      TIMESTAMP NOT NULL,
    deleted     INTEGER NOT NULL DEFAULT 0,
    started_at  TIMESTAMP NOT NULL,
    finished_at TIMESTAMP
);

-- Deletes are allowed only for an open purge of the row's organization and only before its cutoff.
DROP TRIGGER audit_events_no_delete;
-- +goose StatementBegin
CREATE TRIGGER audit_events_no_delete BEFORE DELETE ON audit_events
WHEN NOT EXISTS (SELECT 1 FROM audit_purges p WHERE p.finished_at IS NULL AND p.org_id = OLD.org_id AND OLD.at < p.cutoff)
BEGIN
    SELECT RAISE(ABORT, 'audit_events is append-only');
END;
-- +goose StatementEnd
CREATE INDEX audit_events_org_at ON audit_events (org_id, at);

-- +goose Down
DROP INDEX audit_events_org_at;
DROP TRIGGER audit_events_no_delete;
-- +goose StatementBegin
CREATE TRIGGER audit_events_no_delete BEFORE DELETE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit_events is append-only');
END;
-- +goose StatementEnd
DROP TABLE audit_purges;
DROP TABLE audit_policies;
