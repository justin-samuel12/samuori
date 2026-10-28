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
    updated_at     TIMESTAMP NOT NULL,
    PRIMARY KEY (org_id, recipe_id, recipe_version, label)
);

-- +goose Down
DROP TABLE shipped_runtime_status;
