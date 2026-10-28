-- +goose Up
-- Build output received while an attempt runs, for builds without secrets; removed once the attempt ends
-- (docs/architecture/build-lifecycle.md#live-logs).
CREATE TABLE build_live_logs (
    attempt_id TEXT NOT NULL REFERENCES build_attempts (id),
    generation INTEGER NOT NULL,
    offset_bytes INTEGER NOT NULL,
    data       BLOB NOT NULL,
    created_at TIMESTAMP NOT NULL,
    PRIMARY KEY (attempt_id, offset_bytes)
);
CREATE INDEX build_live_logs_created ON build_live_logs (created_at);

-- +goose Down
DROP TABLE build_live_logs;
