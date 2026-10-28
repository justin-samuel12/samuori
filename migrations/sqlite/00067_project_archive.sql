-- +goose Up
-- A project is active, archived or purged; purged keeps the row, its records and audit trail (ADR 0058).
ALTER TABLE projects ADD COLUMN state TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'archived', 'purged'));
ALTER TABLE projects ADD COLUMN archived_by TEXT;
ALTER TABLE projects ADD COLUMN archived_at TIMESTAMP;
ALTER TABLE projects ADD COLUMN purged_by TEXT;
ALTER TABLE projects ADD COLUMN purged_at TIMESTAMP;

-- +goose Down
ALTER TABLE projects DROP COLUMN purged_at;
ALTER TABLE projects DROP COLUMN purged_by;
ALTER TABLE projects DROP COLUMN archived_at;
ALTER TABLE projects DROP COLUMN archived_by;
ALTER TABLE projects DROP COLUMN state;
