# Domain Model, Persistence and API

Status: **Authoritative** for rules; the entity list is a design candidate to be normalized by ADRs when schemas are
written (M0/M1).

## Persistent identifiers

Every resource has a ULID primary key (sortable, opaque; ADR 0017), an ownership scope (organization → team → project where
applicable), `created_at`/`updated_at`, and a deliberate deletion/retention rule. Persistent IDs exist for:
organization, team, project, source snapshot, spec, spec revision, build run, build attempt, worker, location,
registry, repository, artifact, image digest (subject), tag, publication, promotion (a Publication kind), approval,
policy decision and audit record.

## Candidate tables

`organizations`, `teams`, `memberships`, `projects`, `project_access`, `project_sources`, `source_snapshots`,
`build_specs`, `spec_revisions`, `recipes`, `recipe_versions`, `build_runs`, `build_attempts`, `build_stages`,
`build_events`, `build_artifacts`, `image_subjects`, `image_tags`, `validation_runs`, `check_results`,
`registries`, `registry_repositories`, `publications`, `approvals`, `security_findings`, `security_exceptions`,
`workers`, `worker_certificates`, `worker_pools`, `locations`, `controller_peers`, `storage_profiles`,
`storage_assignments`, `secret_provider_refs`, `messaging_config`, `policies`, `policy_decisions`, `audit_events`,
`notification_rules`, `delivery_attempts`, `outbox`, `backup_runs`, `schema_migrations`, `upgrade_events`.

(`publication_jobs`/`promotions` from the original candidate list are unified into `publications` with a `kind`
column — ADR 0003. `validation_runs`/`check_results` are added by ADR 0003.)

## Rules

- Enforce uniqueness and referential integrity in the schema.
- Separate current mutable metadata (project name, settings) from immutable execution records (snapshots, spec
  revisions, runs, attempts, artifacts, approvals, policy decisions, audit events).
- A project is `active`, `archived` or `purged` (ADR 0058). Archived and purged projects accept no new work; every
  create path checks the state inside its own transaction (`tenancy.RequireActive`). Projects are never deleted.
- Explicit revision numbers and optimistic concurrency (`ETag` / `If-Match`) for spec and policy edits.
- Audit is append-only at the application layer. A SQL row is **not** tamper-proof; where stronger guarantees are
  needed, ship tamper-evident copies externally. Never claim otherwise.
- Tenant-aware model: every row is scoped to an organization even though one organization is supported first (D30).
- Migrations are portable across SQLite and PostgreSQL (separate directories, same logical schema) — ADR 0017.

## Core contracts

- **BuildSpec / SpecRevision** — immutable, portable declarative description of *how to build*. Never contains a
  source snapshot reference (ADR 0004), credentials, AWS accounts, or internal storage choices. See
  [`buildspec.md`](buildspec.md).
- **BuildRun / BuildAttempt** — inputs (`source_snapshot_id` + digest, `spec_revision_id` + digest, recipe version,
  requester, build-plan digest), engine version, worker/location, isolation profile, timestamps, state
  transitions, logs/metrics, resulting OCI digest. See [`build-lifecycle.md`](build-lifecycle.md).
- **ValidationRun** — checks against an image digest.
- **Publication** — destination registry profile, repository, tags, approvals, security gates, immutable artifact
  digest, transfer verification, audit trail.

## API

Stable paths under `/api/v1`, defined in `api/openapi/samuori.v1.yml` (OpenAPI 3.0.3, ADR 0010). Design and endpoint
list: [`api.md`](api.md). Domains:

`/auth`, `/me`, `/organizations`, `/teams`, `/projects`, `/projects/{id}/sources`, `/discovery`, `/specs`,
`/recipes`, `/builds`, `/builds/{id}/events`, `/validations`, `/artifacts`, `/registries`, `/repositories`,
`/publications`, `/images`, `/promotions` (alias view over `kind=promote` publications), `/approvals`, `/workers`,
`/worker-pools`, `/locations`, `/peers`, `/storage-profiles`, `/secrets/providers`, `/policies`, `/notifications`,
`/audit`, `/backups`, `/upgrades`, `/system/health`.

Cross-cutting requirements:

- Pagination (cursor), filtering and sorting conventions defined once and reused.
- Typed error codes (`{code, message, details, request_id}`), deterministic per adapter.
- Per-request idempotency keys for all non-idempotent mutations; durable async operation identifiers.
- Signed, scoped, expiring URLs for artifact download and upload.
- Rate and size limits.
- Browser auth: httpOnly, Secure, SameSite session cookies with CSRF protection; strict CORS.
- API clients: short-lived OAuth/OIDC or service tokens.
- Worker authentication (mTLS identity) **never** grants access to admin or user APIs; workers use a separate,
  narrow worker protocol.
- All channels (UI, CLI, pipelines) share the same authorization, validation and execution paths (D29).
