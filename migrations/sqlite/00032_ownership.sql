-- +goose Up
ALTER TABLE upload_sessions ADD COLUMN owner_controller TEXT;
ALTER TABLE upload_chunks ADD COLUMN digest TEXT;
ALTER TABLE discoveries ADD COLUMN owner_controller TEXT;
ALTER TABLE validation_runs ADD COLUMN owner_controller TEXT;
ALTER TABLE validation_runs ADD COLUMN claim TEXT;
ALTER TABLE destinations ADD COLUMN publishing_by TEXT;
ALTER TABLE pipeline_runs ADD COLUMN owner_controller TEXT;
ALTER TABLE notification_deliveries ADD COLUMN claim TEXT;

-- +goose Down
ALTER TABLE notification_deliveries DROP COLUMN claim;
ALTER TABLE pipeline_runs DROP COLUMN owner_controller;
ALTER TABLE destinations DROP COLUMN publishing_by;
ALTER TABLE validation_runs DROP COLUMN claim;
ALTER TABLE validation_runs DROP COLUMN owner_controller;
ALTER TABLE discoveries DROP COLUMN owner_controller;
ALTER TABLE upload_chunks DROP COLUMN digest;
ALTER TABLE upload_sessions DROP COLUMN owner_controller;
