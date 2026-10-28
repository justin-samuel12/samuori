-- +goose Up
-- Administrator-authored recipe versions (ADR 0041). A version is editable only as a draft.
CREATE TABLE custom_recipes (
    org_id      TEXT NOT NULL REFERENCES organizations (id),
    recipe_id   TEXT NOT NULL,
    version     TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('draft', 'approved', 'deprecated', 'withdrawn')),
    definition  TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMP NOT NULL,
    updated_by  TEXT NOT NULL,
    updated_at  TIMESTAMP NOT NULL,
    PRIMARY KEY (org_id, recipe_id, version)
);

-- +goose Down
DROP TABLE custom_recipes;
