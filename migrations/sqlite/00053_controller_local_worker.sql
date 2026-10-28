-- +goose Up
-- A controller converted from a single server keeps its host's worker, which updates with it (ADR 0046, OD-28).
ALTER TABLE controllers ADD COLUMN local_worker INTEGER NOT NULL DEFAULT 0;
ALTER TABLE rollout_steps ADD COLUMN local_worker_id TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE rollout_steps DROP COLUMN local_worker_id;
ALTER TABLE controllers DROP COLUMN local_worker;
