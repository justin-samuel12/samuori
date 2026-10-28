# 0036. Version matrix and upgrades

Status: Accepted (2026-10-20). Implements D28.

## Context
D28 asks for a supported version matrix, worker draining and rolling upgrades, controller migration safeguards,
schema evolution, compatibility checks and audited updates.

## Decision
- **Matrix.** Releases use semantic versions. A worker is accepted when it runs the controller's major and minor
  version or the minor version before it; a newer worker, another major version or a gap of two minor versions makes
  it ineligible (`version-incompatible`). Controllers sharing a database may be one minor version apart during a
  rolling upgrade. Development builds work only with development builds. The protocol major version must match.
- **Order.** Controllers upgrade first, one at a time; workers follow by draining, upgrading and reconnecting.
- **Schema.** The first upgraded controller migrates, under PostgreSQL's session lock so that controllers starting
  together never migrate at once. A controller refuses a schema newer than it knows. Migrations within a minor
  release are additive (new tables, nullable or defaulted columns), so the previous minor release keeps working on
  the new schema; removing or narrowing anything waits for the next minor release.
- **Safeguards.** With backups configured, a controller that is about to migrate an existing installation first writes
  a backup (`samuori-before-schema-N-….age`, trigger `upgrade`); a failed backup stops the upgrade. Every applied upgrade
  is audited (`upgrade.migrate`, from and to schema and release).
- **Visibility.** The Upgrades & Diagnostics screen and `GET /system/upgrades` show each controller's release, the
  schema, worker releases against the matrix and the upgrade history; the `controller-versions` health check warns
  when a live controller is outside the window.

## Consequences
- Rollback is a restore of the pre-upgrade backup into the previous release; a newer schema is never used by an
  older binary.
- The additive-migration rule is a review obligation (`docs/developers/git-workflow.md`), backed by tests that
  migrate from every earlier schema version on both dialects.
