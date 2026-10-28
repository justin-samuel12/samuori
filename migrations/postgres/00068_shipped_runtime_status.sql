-- +goose Up
-- An organization's decision about one shipped runtime of a built-in recipe version (ADR 0060). No row means approved.
CREATE TABLE shipped_runtime_status (
    org_id         TEXT NOT NULL REFERENCES organizations (id),
    recipe_id      TEXT NOT NULL,
    recipe_version TEXT NOT NULL,
    label          TEXT NOT NULL,
    status         TEXT NOT NULL CHECK (status IN ('deprecated', 'withdrawn')),
    reason         TEXT NOT NULL,
    updated_by     TEXT NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (org_id, recipe_id, recipe_version, label)
);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON shipped_runtime_status TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE shipped_runtime_status;
