-- +goose Up
-- A requester may withdraw a pending request; a deleted pipeline keeps its row and run history (ADR 0062).
ALTER TABLE approvals DROP CONSTRAINT approvals_state_check;
ALTER TABLE approvals ADD CONSTRAINT approvals_state_check CHECK (state IN ('pending', 'approved', 'rejected', 'expired', 'withdrawn'));

ALTER TABLE pipelines ADD COLUMN deleted_by TEXT;
ALTER TABLE pipelines ADD COLUMN deleted_at TIMESTAMPTZ;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'samuori_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON kube_conversions, kube_conversion_runs, kube_conversion_revisions, kube_conversion_files TO samuori_app;
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose Down
ALTER TABLE pipelines DROP COLUMN deleted_at;
ALTER TABLE pipelines DROP COLUMN deleted_by;
UPDATE approvals SET state = 'expired' WHERE state = 'withdrawn';
ALTER TABLE approvals DROP CONSTRAINT approvals_state_check;
ALTER TABLE approvals ADD CONSTRAINT approvals_state_check CHECK (state IN ('pending', 'approved', 'rejected', 'expired'));
