-- +goose Up
-- Administrator approval of optional build and conversion providers, and the build method policy (ADR 0047, 0048).
CREATE TABLE providers (
    org_id     TEXT NOT NULL REFERENCES organizations (id),
    name       TEXT NOT NULL,
    config     TEXT NOT NULL,
    updated_by TEXT NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    PRIMARY KEY (org_id, name)
);

CREATE TABLE build_method_policies (
    org_id         TEXT PRIMARY KEY REFERENCES organizations (id),
    allowed        TEXT NOT NULL,
    default_method TEXT NOT NULL DEFAULT '',
    updated_by     TEXT NOT NULL,
    updated_at     TIMESTAMP NOT NULL
);

-- +goose Down
DROP TABLE build_method_policies;
DROP TABLE providers;
