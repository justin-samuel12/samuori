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
    expires_at      TIMESTAMP NOT NULL,
    created_at      TIMESTAMP NOT NULL,
    decided_at      TIMESTAMP
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
    created_at  TIMESTAMP NOT NULL,
    retired_at  TIMESTAMP,
    UNIQUE (org_id, name)
);

ALTER TABLE destinations ADD COLUMN sign_images INTEGER NOT NULL DEFAULT 0;
ALTER TABLE destinations ADD COLUMN attach_provenance INTEGER NOT NULL DEFAULT 0;
ALTER TABLE destinations ADD COLUMN promotion_only INTEGER NOT NULL DEFAULT 0;
-- SQLite cannot change a CHECK constraint, so publications is rebuilt to allow rollbacks.
CREATE TABLE publications_new (
    id                    TEXT PRIMARY KEY,
    org_id                TEXT NOT NULL REFERENCES organizations (id),
    project_id            TEXT NOT NULL REFERENCES projects (id),
    kind                  TEXT NOT NULL CHECK (kind IN ('publish', 'promote', 'rollback')),
    artifact_id           TEXT NOT NULL REFERENCES build_artifacts (id),
    artifact_digest       TEXT NOT NULL,
    destination_id        TEXT NOT NULL REFERENCES destinations (id),
    tags                  TEXT NOT NULL,
    state                 TEXT NOT NULL,
    reason                TEXT,
    gate_decisions        TEXT NOT NULL,
    image_digest          TEXT,
    destination_ref       TEXT,
    requested_by          TEXT NOT NULL,
    idempotency_key       TEXT NOT NULL,
    created_at            TIMESTAMP NOT NULL,
    updated_at            TIMESTAMP NOT NULL,
    approval_id           TEXT,
    source_publication_id TEXT REFERENCES publications (id),
    signature_ref         TEXT,
    attestation_ref       TEXT,
    validation_run_id     TEXT,
    deprecated_at         TIMESTAMP,
    deprecation_reason    TEXT,
    retired_at            TIMESTAMP,
    retired_by            TEXT,
    retirement_reason     TEXT,
    UNIQUE (requested_by, idempotency_key)
);
INSERT INTO publications_new (id, org_id, project_id, kind, artifact_id, artifact_digest, destination_id, tags, state, reason, gate_decisions, image_digest, destination_ref, requested_by, idempotency_key, created_at, updated_at, approval_id) SELECT id, org_id, project_id, kind, artifact_id, artifact_digest, destination_id, tags, state, reason, gate_decisions, image_digest, destination_ref, requested_by, idempotency_key, created_at, updated_at, approval_id FROM publications;
DROP TABLE publications;
ALTER TABLE publications_new RENAME TO publications;
CREATE INDEX publications_state ON publications (state);
CREATE INDEX publications_artifact ON publications (artifact_id);
CREATE INDEX publications_destination ON publications (destination_id, state);

-- +goose Down
CREATE TABLE publications_old (
    id                    TEXT PRIMARY KEY,
    org_id                TEXT NOT NULL REFERENCES organizations (id),
    project_id            TEXT NOT NULL REFERENCES projects (id),
    kind                  TEXT NOT NULL CHECK (kind IN ('publish', 'promote')),
    artifact_id           TEXT NOT NULL REFERENCES build_artifacts (id),
    artifact_digest       TEXT NOT NULL,
    destination_id        TEXT NOT NULL REFERENCES destinations (id),
    tags                  TEXT NOT NULL,
    state                 TEXT NOT NULL,
    reason                TEXT,
    gate_decisions        TEXT NOT NULL,
    image_digest          TEXT,
    destination_ref       TEXT,
    requested_by          TEXT NOT NULL,
    idempotency_key       TEXT NOT NULL,
    created_at            TIMESTAMP NOT NULL,
    updated_at            TIMESTAMP NOT NULL,
    approval_id           TEXT,
    UNIQUE (requested_by, idempotency_key)
);
INSERT INTO publications_old (id, org_id, project_id, kind, artifact_id, artifact_digest, destination_id, tags, state, reason, gate_decisions, image_digest, destination_ref, requested_by, idempotency_key, created_at, updated_at, approval_id) SELECT id, org_id, project_id, kind, artifact_id, artifact_digest, destination_id, tags, state, reason, gate_decisions, image_digest, destination_ref, requested_by, idempotency_key, created_at, updated_at, approval_id FROM publications WHERE kind IN ('publish', 'promote');
DROP TABLE publications;
ALTER TABLE publications_old RENAME TO publications;
CREATE INDEX publications_state ON publications (state);
CREATE INDEX publications_artifact ON publications (artifact_id);
ALTER TABLE destinations DROP COLUMN promotion_only;
ALTER TABLE destinations DROP COLUMN attach_provenance;
ALTER TABLE destinations DROP COLUMN sign_images;
DROP TABLE signing_keys;
ALTER TABLE check_results DROP COLUMN vulnerabilities;
ALTER TABLE validation_runs DROP COLUMN classification;
DROP TABLE output_exceptions;
