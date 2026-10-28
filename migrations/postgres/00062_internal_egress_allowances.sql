-- +goose Up
-- A project allowance may open an internal destination, one that resolves to a private address (ADR 0052 amendment).
ALTER TABLE project_egress_allowances ADD COLUMN internal BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE project_egress_allowances DROP COLUMN internal;
