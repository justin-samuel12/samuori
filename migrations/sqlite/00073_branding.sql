-- +goose Up
-- One operator logo per installation, keyed by organization (ADR 0072). Image bytes live in the blob store; only the
-- content digest, media type and intrinsic size are kept here.
CREATE TABLE branding (
    org_id               TEXT PRIMARY KEY REFERENCES organizations (id),
    logo_digest          TEXT,
    logo_content_type    TEXT,
    logo_width           INTEGER,
    logo_height          INTEGER,
    logo_dark_digest     TEXT,
    logo_dark_content_type TEXT,
    logo_dark_width      INTEGER,
    logo_dark_height     INTEGER,
    updated_by           TEXT NOT NULL,
    updated_at           TIMESTAMP NOT NULL
);

-- +goose Down
DROP TABLE branding;
