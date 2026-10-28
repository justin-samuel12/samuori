-- +goose Up
-- Destinations the worker's egress proxy denied, reported as structured data (docs/architecture/egress.md#denied-destinations).
ALTER TABLE build_attempts ADD COLUMN egress_report TEXT;

-- +goose Down
ALTER TABLE build_attempts DROP COLUMN egress_report;
