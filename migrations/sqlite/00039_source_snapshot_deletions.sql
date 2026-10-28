-- +goose Up
-- The uploader may delete an upload; its row stays for build history and its files are removed (ADR 0040).
CREATE TABLE source_snapshot_deletions (
    snapshot_id TEXT PRIMARY KEY REFERENCES source_snapshots (id),
    deleted_by  TEXT NOT NULL,
    deleted_at  TIMESTAMP NOT NULL
);

-- +goose Down
DROP TABLE source_snapshot_deletions;
