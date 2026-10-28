-- +goose Up
CREATE TABLE outbox (
    seq          BIGSERIAL PRIMARY KEY,
    msg_id       TEXT NOT NULL UNIQUE,
    subject      TEXT NOT NULL,
    payload      BYTEA NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ
);
CREATE INDEX outbox_unpublished ON outbox (seq) WHERE published_at IS NULL;

CREATE TABLE audit_events (
    id            TEXT PRIMARY KEY,
    at            TIMESTAMPTZ NOT NULL,
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
CREATE FUNCTION audit_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER audit_events_no_update_delete BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_append_only();
CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only();

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON outbox TO samuori_app;
        GRANT USAGE ON SEQUENCE outbox_seq_seq TO samuori_app;
        GRANT SELECT, INSERT ON audit_events TO samuori_app;
        REVOKE UPDATE, DELETE, TRUNCATE ON audit_events FROM samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE audit_events;
DROP FUNCTION audit_events_append_only();
DROP TABLE outbox;
