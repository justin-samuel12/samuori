-- +goose Up
CREATE TABLE audit_policies (
    org_id            TEXT PRIMARY KEY REFERENCES organizations (id),
    retention_days    INTEGER NOT NULL DEFAULT 0,
    export_enabled    BOOLEAN NOT NULL DEFAULT FALSE,
    require_export    BOOLEAN NOT NULL DEFAULT TRUE,
    exported_through  TIMESTAMPTZ,
    last_export_at    TIMESTAMPTZ,
    last_export_error TEXT,
    last_purge_at     TIMESTAMPTZ,
    last_purge_count  INTEGER NOT NULL DEFAULT 0,
    updated_by        TEXT NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);

CREATE TABLE audit_purges (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations (id),
    cutoff      TIMESTAMPTZ NOT NULL,
    deleted     INTEGER NOT NULL DEFAULT 0,
    started_at  TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ
);
CREATE INDEX audit_events_org_at ON audit_events (org_id, at);

-- The app role still cannot delete audit rows. Only samuori_audit_purge, run as the owner, may, and never for events
-- younger than 30 days (docs/architecture/observability.md#retention).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION audit_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND current_setting('samuori.audit_purge', true) = 'on' THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit_events is append-only';
END;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION samuori_audit_purge(p_org TEXT, p_cutoff TIMESTAMPTZ) RETURNS BIGINT
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    n BIGINT;
BEGIN
    IF p_cutoff > now() - INTERVAL '30 days' THEN
        RAISE EXCEPTION 'audit events younger than 30 days are never purged';
    END IF;
    PERFORM set_config('samuori.audit_purge', 'on', true);
    DELETE FROM audit_events WHERE org_id = p_org AND at < p_cutoff;
    GET DIAGNOSTICS n = ROW_COUNT;
    PERFORM set_config('samuori.audit_purge', 'off', true);
    RETURN n;
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION samuori_audit_purge(TEXT, TIMESTAMPTZ) FROM PUBLIC;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE ON audit_policies, audit_purges TO samuori_app;
        GRANT EXECUTE ON FUNCTION samuori_audit_purge(TEXT, TIMESTAMPTZ) TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION samuori_audit_purge(TEXT, TIMESTAMPTZ);
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION audit_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only';
END;
$$;
-- +goose StatementEnd
DROP INDEX audit_events_org_at;
DROP TABLE audit_purges;
DROP TABLE audit_policies;
