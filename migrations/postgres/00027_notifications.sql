-- +goose Up
CREATE TABLE notification_channels (
    id                    TEXT PRIMARY KEY,
    org_id                TEXT NOT NULL REFERENCES organizations (id),
    name                  TEXT NOT NULL,
    kind                  TEXT NOT NULL CHECK (kind IN ('webhook', 'slack', 'teams', 'email')),
    config                TEXT NOT NULL,
    secret_id             TEXT,
    allow_private_network BOOLEAN NOT NULL DEFAULT FALSE,
    enabled               BOOLEAN NOT NULL DEFAULT TRUE,
    created_by            TEXT NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL,
    UNIQUE (org_id, name)
);

CREATE TABLE notification_rules (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    channel_id TEXT NOT NULL REFERENCES notification_channels (id),
    events     TEXT NOT NULL,
    team_id    TEXT,
    project_id TEXT,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX notification_rules_org ON notification_rules (org_id);

CREATE TABLE notification_deliveries (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL REFERENCES organizations (id),
    channel_id      TEXT NOT NULL REFERENCES notification_channels (id),
    event_id        TEXT NOT NULL,
    event_type      TEXT NOT NULL,
    message         TEXT NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('pending', 'delivered', 'dead')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL,
    delivered_at    TIMESTAMPTZ,
    UNIQUE (channel_id, event_id)
);
CREATE INDEX notification_deliveries_due ON notification_deliveries (state, next_attempt_at);

CREATE TABLE notifications (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    user_id    TEXT NOT NULL,
    event_id   TEXT NOT NULL,
    event_type TEXT NOT NULL,
    message    TEXT NOT NULL,
    read_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (user_id, event_id)
);
CREATE INDEX notifications_user ON notifications (user_id, created_at);

CREATE TABLE notification_cursor (
    id    INTEGER PRIMARY KEY,
    at    TIMESTAMPTZ NOT NULL,
    event TEXT NOT NULL
);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON notification_channels, notification_rules, notification_deliveries, notifications, notification_cursor TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE notification_cursor;
DROP TABLE notifications;
DROP TABLE notification_deliveries;
DROP TABLE notification_rules;
DROP TABLE notification_channels;
