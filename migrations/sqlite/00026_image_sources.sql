-- +goose Up
CREATE TABLE image_sources (
    id                  TEXT PRIMARY KEY,
    org_id              TEXT NOT NULL REFERENCES organizations (id),
    name                TEXT NOT NULL,
    upstream            TEXT NOT NULL,
    mirror              TEXT NOT NULL,
    repository_patterns TEXT NOT NULL,
    team_ids            TEXT NOT NULL,
    pull_profile_id     TEXT REFERENCES registry_profiles (id),
    status              TEXT NOT NULL CHECK (status IN ('active', 'disabled')),
    created_by          TEXT NOT NULL,
    created_at          TIMESTAMP NOT NULL,
    updated_at          TIMESTAMP NOT NULL,
    UNIQUE (org_id, name)
);

CREATE TABLE mirror_token_keys (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations (id),
    certificate TEXT NOT NULL,
    secret_id   TEXT NOT NULL,
    state       TEXT NOT NULL CHECK (state IN ('active', 'retired')),
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMP NOT NULL,
    retired_at  TIMESTAMP
);

-- +goose Down
DROP TABLE mirror_token_keys;
DROP TABLE image_sources;
