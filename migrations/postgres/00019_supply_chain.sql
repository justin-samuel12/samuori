-- +goose Up
CREATE TABLE output_exceptions (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL REFERENCES organizations (id),
    project_id      TEXT NOT NULL REFERENCES projects (id),
    classes         TEXT NOT NULL,
    path_patterns   TEXT NOT NULL,
    justification   TEXT NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('requested', 'approved', 'rejected', 'revoked')),
    requested_by    TEXT NOT NULL,
    decided_by      TEXT NOT NULL DEFAULT '',
    decision_reason TEXT NOT NULL DEFAULT '',
    expires_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    decided_at      TIMESTAMPTZ
);
CREATE INDEX output_exceptions_project ON output_exceptions (project_id, state);

ALTER TABLE validation_runs ADD COLUMN classification TEXT NOT NULL DEFAULT 'standard';
ALTER TABLE check_results ADD COLUMN vulnerabilities TEXT NOT NULL DEFAULT '[]';

CREATE TABLE signing_keys (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations (id),
    name        TEXT NOT NULL,
    public_key  TEXT NOT NULL,
    secret_id   TEXT NOT NULL,
    state       TEXT NOT NULL CHECK (state IN ('active', 'retired')),
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    retired_at  TIMESTAMPTZ,
    UNIQUE (org_id, name)
);

ALTER TABLE destinations ADD COLUMN sign_images BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE destinations ADD COLUMN attach_provenance BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE destinations ADD COLUMN promotion_only BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE publications ADD COLUMN source_publication_id TEXT REFERENCES publications (id);
ALTER TABLE publications ADD COLUMN signature_ref TEXT;
ALTER TABLE publications ADD COLUMN attestation_ref TEXT;
ALTER TABLE publications ADD COLUMN validation_run_id TEXT;
ALTER TABLE publications ADD COLUMN deprecated_at TIMESTAMPTZ;
ALTER TABLE publications ADD COLUMN deprecation_reason TEXT;
ALTER TABLE publications ADD COLUMN retired_at TIMESTAMPTZ;
ALTER TABLE publications ADD COLUMN retired_by TEXT;
ALTER TABLE publications ADD COLUMN retirement_reason TEXT;
ALTER TABLE publications DROP CONSTRAINT publications_kind_check;
ALTER TABLE publications ADD CONSTRAINT publications_kind_check CHECK (kind IN ('publish', 'promote', 'rollback'));
CREATE INDEX publications_destination ON publications (destination_id, state);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE ON output_exceptions, signing_keys TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP INDEX publications_destination;
DELETE FROM publications WHERE kind = 'rollback';
ALTER TABLE publications DROP CONSTRAINT publications_kind_check;
ALTER TABLE publications ADD CONSTRAINT publications_kind_check CHECK (kind IN ('publish', 'promote'));
ALTER TABLE publications DROP COLUMN retirement_reason;
ALTER TABLE publications DROP COLUMN retired_by;
ALTER TABLE publications DROP COLUMN retired_at;
ALTER TABLE publications DROP COLUMN deprecation_reason;
ALTER TABLE publications DROP COLUMN deprecated_at;
ALTER TABLE publications DROP COLUMN validation_run_id;
ALTER TABLE publications DROP COLUMN attestation_ref;
ALTER TABLE publications DROP COLUMN signature_ref;
ALTER TABLE publications DROP COLUMN source_publication_id;
ALTER TABLE destinations DROP COLUMN promotion_only;
ALTER TABLE destinations DROP COLUMN attach_provenance;
ALTER TABLE destinations DROP COLUMN sign_images;
DROP TABLE signing_keys;
ALTER TABLE check_results DROP COLUMN vulnerabilities;
ALTER TABLE validation_runs DROP COLUMN classification;
DROP TABLE output_exceptions;
