-- +goose Up
-- Session idle timeout, maximum length and warning lead are administrator settings (ADR 0045).
ALTER TABLE organizations ADD COLUMN session_idle_seconds INTEGER NOT NULL DEFAULT 1800;
ALTER TABLE organizations ADD COLUMN session_max_seconds INTEGER NOT NULL DEFAULT 43200;
ALTER TABLE organizations ADD COLUMN session_warning_seconds INTEGER NOT NULL DEFAULT 120;

-- +goose Down
ALTER TABLE organizations DROP COLUMN session_warning_seconds;
ALTER TABLE organizations DROP COLUMN session_max_seconds;
ALTER TABLE organizations DROP COLUMN session_idle_seconds;
