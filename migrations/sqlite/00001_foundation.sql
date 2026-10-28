-- +goose Up
CREATE TABLE outbox (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    msg_id       TEXT NOT NULL UNIQUE,
    subject      TEXT NOT NULL,
    payload      BLOB NOT NULL,
    created_at   TIMESTAMP NOT NULL,
    published_at TIMESTAMP
);
CREATE INDEX outbox_unpublished ON outbox (seq) WHERE published_at IS NULL;

CREATE TABLE audit_events (
    id            TEXT PRIMARY KEY,
    at            TIMESTAMP NOT NULL,
    org_id        TEXT,
    actor_type    TEXT NOT NULL,
    actor_id      TEXT,
    action        TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id   TEXT,
    outcome       TEXT NOT NULL,
    reason        TEXT,
    request_id    TEXT
);
CREATE INDEX audit_events_resource ON audit_events (resource_type, resource_id);
CREATE INDEX audit_events_at ON audit_events (at);

-- +goose StatementBegin
CREATE TRIGGER audit_events_no_update BEFORE UPDATE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit_events is append-only');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER audit_events_no_delete BEFORE DELETE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit_events is append-only');
END;
-- +goose StatementEnd

-- +goose Down
DROP TABLE audit_events;
DROP TABLE outbox;
