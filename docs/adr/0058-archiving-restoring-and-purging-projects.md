# 0058. Archiving, restoring and purging projects

Status: **Accepted** (2026-10-28, owner decision; provisional, may change during acceptance testing). Extends
[ADR 0053](0053-image-download-and-build-deletion.md) and [ADR 0040](0040-uploader-deletes-uploads.md).

## Context
A project could be created but never removed. Old projects stayed in every list, kept their secret grants, egress
allowances and destination access, and kept everything they stored. The owner asked for a reversible way to retire a
project and an irreversible way to free its storage.

## Options
- **Removal:** archive, then optionally purge (chosen); delete the project row and everything under it (rejected:
  audit events, spec revisions and published images' provenance must stay traceable, and several tables are
  immutable); archive only (rejected: storage is never freed).
- **New work on an archived project:** one guard that every create path calls inside its transaction (chosen);
  checks in each handler (rejected: easy to miss one) or a database trigger (rejected: it would also fail the
  background transactions that finish work already running).
- **Running builds when archiving:** cancel them through the ordinary cancel path (chosen: the build would otherwise
  use grants that archiving revokes, and the worker is told to stop); let them finish (rejected).
- **Grants on restore:** stay revoked (chosen: a restored project starts with no access and an administrator grants
  it again); come back (rejected: access would return without anyone deciding it again).
- **Purge execution:** synchronous with a typed confirmation and safe to repeat (chosen: it marks records and deletes
  files, which is quick enough for one request); a background operation (rejected for now: no long-running step).

## Decision
- **State.** `projects.state` is `active`, `archived` or `purged`, with who and when for archive and purge (migration
  00067). `GET /projects` returns active projects; `include=archived` adds archived and purged ones the caller can see.
- **Archive** (`POST /projects/{id}/archive`, optional reason): the project's maintainers and administrators. In one
  transaction it sets the state and revokes, each audited: secret grants and bindings, egress allowances, the
  project's place in every destination's allowed projects, federation grants, pipelines (disabled) and approved or
  requested output exceptions; pending approvals and requests expire through their normal expiry handlers. Then it
  cancels unfinished builds with the ordinary cancel (audited `build.cancel`) and closes open uploads. Validations,
  publications already pushing, discoveries and conversions already running finish; publications waiting for
  approval expire, and queued ones are refused by the gate because the destination is closed to the project.
  Audited `project.archive`.
- **Refusal.** `tenancy.RequireActive` runs inside the transaction of every path that adds work: uploads and
  snapshots, specs, spec revisions and drafts, discoveries, builds, validation re-runs, publications (and automatic
  publication is skipped), conversions and their re-runs and edits, secret grants and bindings, output exceptions,
  every approval request and pipeline runs, and project settings. The API answers `409 project_archived`.
- **Restore** (`POST /projects/{id}/restore`): the same roles; only from `archived`; refused for `purged`. The
  project has no grants afterwards. Audited `project.restore`.
- **Purge** (`POST /projects/{id}/purge`): organization administrators, only for an archived (or already purged)
  project, with `X-Samuori-Confirm` set to the project's name (428 otherwise), and refused with 409 while any build,
  validation, publication, discovery, conversion or upload commit of the project is still running. It marks every
  upload and build deleted (`source_snapshot_deletions`, `build_run_deletions`), deletes cache entries, live output,
  staged upload chunks and wizard drafts, clears discovery results and conversion file contents, sets `purged` and is
  audited (`project.purge`). It then removes from storage the snapshot manifests and files, build images, logs and
  reports, and validation outputs such as SBOMs that nothing else references, and audits the outcome
  (`project.purge.storage`). Content another project, a hold or a release still uses is kept.
- **Kept.** The project row and name, every record with its IDs, digests and times (builds, attempts, artifacts,
  validations and check results, publications, spec revisions, uploads) and the audit trail, so the provenance of
  images already published stays traceable. Images already pushed to external registries are **not** deleted; the
  response and the UI say so.
- **Partial failure.** The state is set before files are removed; if removal fails the API answers 500
  `purge_incomplete`, the audit records it, and the purge can be run again. Garbage collection also stops keeping the
  purged project's files, so a failure that is never retried is cleaned up at a later collection.

## Consequences
- Archiving is reversible, but grants are not: restoring needs an administrator to grant access again.
- Purging is not reversible. A purged project's builds show as deleted and its uploads as deleted.
- Not yet: a purge that runs as a background operation, and retention rules that archive or purge automatically.
- Reversible: removing the endpoints leaves the new columns unread; `active` is the default.
