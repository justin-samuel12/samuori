-- +goose Up
-- Workers being removed: drained, then their host wiped, then revoked (ADR 0045).
CREATE TABLE worker_removals (
    worker_id    TEXT PRIMARY KEY REFERENCES workers (id),
    org_id       TEXT NOT NULL,
    reason       TEXT NOT NULL,
    requested_by TEXT NOT NULL,
    requested_at TIMESTAMP NOT NULL,
    state        TEXT NOT NULL CHECK (state IN ('draining', 'wiping', 'removed')),
    outcome      TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('', 'wiped', 'not-confirmed')),
    detail       TEXT NOT NULL DEFAULT '',
    updated_at   TIMESTAMP NOT NULL
);

-- +goose Down
DROP TABLE worker_removals;
