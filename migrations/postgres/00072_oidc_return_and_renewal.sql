-- +goose Up
-- Where an OIDC sign-in returns, and the session it renews, if any (ADR 0068).
ALTER TABLE oidc_logins ADD COLUMN return_to TEXT NOT NULL DEFAULT '/';
ALTER TABLE oidc_logins ADD COLUMN renew_session_hash TEXT;
ALTER TABLE oidc_logins ADD COLUMN renew_user_id TEXT;

-- +goose Down
ALTER TABLE oidc_logins DROP COLUMN renew_user_id;
ALTER TABLE oidc_logins DROP COLUMN renew_session_hash;
ALTER TABLE oidc_logins DROP COLUMN return_to;
