-- +goose Up
-- Disk, memory and CPUs each controller and worker last reported, as JSON (docs/operators/capacity.md).
ALTER TABLE controllers ADD COLUMN capacity TEXT;
ALTER TABLE controllers ADD COLUMN capacity_at TIMESTAMP;
ALTER TABLE workers ADD COLUMN capacity TEXT;
ALTER TABLE workers ADD COLUMN capacity_at TIMESTAMP;

-- +goose Down
ALTER TABLE workers DROP COLUMN capacity_at;
ALTER TABLE workers DROP COLUMN capacity;
ALTER TABLE controllers DROP COLUMN capacity_at;
ALTER TABLE controllers DROP COLUMN capacity;
