-- +goose Up
-- Workers enrol unassigned and are assigned to a pool later; portal tokens name no pool (ADR 0045).
ALTER TABLE workers ALTER COLUMN pool_id DROP NOT NULL;
ALTER TABLE enrollment_tokens ALTER COLUMN pool_id DROP NOT NULL;

-- Every organization starts with one location, default, unless it already has locations.
INSERT INTO locations (id, org_id, name, description, created_at)
    SELECT 'loc-' || o.id || '-default', o.id, 'default', '', o.created_at FROM organizations o
    WHERE NOT EXISTS (SELECT 1 FROM locations l WHERE l.org_id = o.id);

-- +goose Down
DELETE FROM worker_removals WHERE worker_id IN (SELECT id FROM workers WHERE pool_id IS NULL);
DELETE FROM workers WHERE pool_id IS NULL;
DELETE FROM enrollment_tokens WHERE pool_id IS NULL;
ALTER TABLE enrollment_tokens ALTER COLUMN pool_id SET NOT NULL;
ALTER TABLE workers ALTER COLUMN pool_id SET NOT NULL;
