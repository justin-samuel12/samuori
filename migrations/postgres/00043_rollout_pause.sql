-- +goose Up
-- A rollout may stop after its first worker so an administrator can check it before the others (ADR 0044).
ALTER TABLE rollouts ADD COLUMN pause_after INTEGER;

-- +goose Down
ALTER TABLE rollouts DROP COLUMN pause_after;
