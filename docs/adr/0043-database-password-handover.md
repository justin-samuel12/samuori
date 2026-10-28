# 0043. Database password handover from the UI

Status: Accepted (2026-10-22) — owner decision

## Context
Controllers read the PostgreSQL DSN from `/etc/samuori/env`. Rotating the password meant editing that file on every
controller and restarting each one. Administrators want to check the connection and hand over a new password from
the portal. The owner decided that Samuori never changes its own password: the database (for example RDS) stays the
only place where the password is set.

## Decision
- **Administration > Database** shows the target (host, database, user, `sslmode`), the server version, query time,
  pool use, the newest password revision and each live controller's check of it. It never shows a password.
  **Test connection** signs in with a new connection from the controller handling the request.
- **Update password** (administrators, with a reason, audited as `database.password.update`): the handling controller
  signs in with the new password on a fresh connection first; a refusal changes nothing. It then switches its own
  new connections to the password, stores the revision in `database_credentials` sealed with a key derived from the
  cluster key (AES-GCM, bound to the revision), and writes it to `<dataDir>/database-password` (0600).
- Every controller polls for a newer revision every 5 s, signs in with it on a fresh connection, and only then
  writes the file and switches; it records `verified` or `failed` with the reason in `database_credential_checks` and
  retries a failure a minute later. Open connections keep the password they signed in with.
- At start, `<dataDir>/database-password` replaces the password in the configured DSN, for the server, preflight,
  migrations and backups. The configured DSN is never rewritten.
- A controller stopped during a change cannot sign in when it starts. `samuori-server set-database-password` (password on
  standard input, `--env-file /etc/samuori/env`) verifies the password and stores it for that controller alone.
- NATS never carries the password, and it is never logged or returned by the API.

## Consequences
- Rotation has a window: between changing the password at the database and entering it here, new connections fail;
  existing ones keep working.
- A separate `database.migrationDsn` with its own credentials is not covered; rotate it by hand.
- A dead pooled connection no longer fails the next read or transaction: those retry on another connection (amended
  2026-10-23, `operators/database.md#lost-connections`). A write outside a transaction can still fail once.
