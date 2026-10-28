-- +goose Up
ALTER TABLE pool_self_tests ADD COLUMN profile TEXT NOT NULL DEFAULT '';
ALTER TABLE pool_self_tests ADD COLUMN config_digest TEXT NOT NULL DEFAULT '';
CREATE INDEX pool_self_tests_worker ON pool_self_tests (worker_id, started_at);

-- +goose Down
DROP INDEX pool_self_tests_worker;
ALTER TABLE pool_self_tests DROP COLUMN config_digest;
ALTER TABLE pool_self_tests DROP COLUMN profile;
