# The 31 Product Decisions

Status: **Authoritative.** These are mandatory product requirements established by the owner. Implementation
status for each lives in [`../traceability.md`](../traceability.md). Change a decision only through a new
owner-approved ADR; never silently.

Where later ADRs clarify *how* a decision is realized, the clarification is linked; the decision text is unchanged.
Use open-source capabilities where appropriate and implement only the required glue.

| ID | Area | Decision | Clarifying ADRs |
|---|---|---|---|
| D01 | Publishing | Per-project manual or automatic publishing; admin policies and approvals override user preference. | [0003](../adr/0003-separate-build-validation-publication-lifecycles.md) |
| D02 | Tags | Unique immutable build tag automatically; optional multiple/custom/semantic tags; templates, protected/moving-tag rules, collision handling and immutable-digest identity. Amended by ADR 0054 (owner, 2026-10-27): successful builds get a `<UTC date>.<n>` version, used as the default tag. | ADR 0054 |
| D03 | ECR | Admins discover/register/create/configure repositories and user access; developers select approved destinations/request new ones; AWS IAM remains authoritative. | — |
| D04 | Storage | Admin-configured multiple local/NFS/EFS/S3/S3-compatible/OCI backends, policy-based routing by data type/location/retention/capacity, admin overrides; developers cannot select internal storage. | — |
| D05 | Engines | Pluggable; BuildKit/Buildx first; Buildah/Podman and Kubernetes-native/Shipwright adapters; explicit platform support. | [0013](../adr/0013-buildkit-default-engine-and-dev-environment.md) |
| D06 | Nodes | One product release configured as controller, worker or all-in-one; enrollment and future clustering. | [0012](../adr/0012-node-roles-and-process-model.md) |
| D07 | DB | SQLite for standalone/single controller; PostgreSQL required for multi-controller HA; safeguarded SQLite-to-PostgreSQL migration and verified recovery. | [0017](../adr/0017-persistence-and-messaging.md) |
| D08 | Queue | Embedded NATS/JetStream default; optional external clustered NATS; DB-authoritative state, durable messages and crash recovery. | [0017](../adr/0017-persistence-and-messaging.md) |
| D09 | Identity | Local, LDAP/AD, OIDC, group mapping, central scoped RBAC, audited service identities and restricted recovery access. | — |
| D10 | Secrets | Local encrypted, Vault/OpenBao optional, AWS Secrets Manager; admin policies, scoped secret references and just-in-time access. | [0015](../adr/0015-secrets-providers-openbao-preferred.md) |
| D11 | Enrollment | Short-lived single-use tokens; worker-initiated outbound connections, pinned controller identity, unique rotated/revocable mTLS IDs, optional Vault PKI (OpenBao PKI preferred per ADR 0015). | [0015](../adr/0015-secrets-providers-openbao-preferred.md) |
| D12 | Scheduler | Match capabilities, OS/arch/engine, quotas, capacity, priorities, location and policy; authorized preferences; fair queueing and fenced dispatch. | — |
| D13 | Discovery | Extensible evidence-based detectors; generate spec, guided review, Guided/Detailed/YAML views and preservation of advanced constructs. | [0005](../adr/0005-editor-modes-guided-detailed-yaml.md) |
| D14 | Validation | Application-based profiles, admin required checks, optional project tests, sandboxed startup/health checks, accurately reported skipped/blocked outcomes. | [0003](../adr/0003-separate-build-validation-publication-lifecycles.md) |
| D15 | Security | Scanning, SBOM, signing, provenance, policy gates, audited time-limited exceptions, rescan and promotion revalidation. | [0003](../adr/0003-separate-build-validation-publication-lifecycles.md) |
| D16 | Recipes | Admin-maintained, approved, versioned built-in/custom recipes, base image policies and reusable steps; controlled overrides/deprecation/version upgrades. | — |
| D17 | Sources | Immutable versioned snapshots/content digests with admin retention; the uploader may delete their own upload (its record stays, its files go); optional future browser workspace, not a source-control product. | [0004](../adr/0004-source-snapshot-is-buildrun-input.md), [0040](../adr/0040-uploader-deletes-uploads.md) |
| D18 | Observability | Searchable build history, user attribution, image lineage, worker/location metrics, audit, failure diagnostics, RBAC, configurable retention. | [0016](../adr/0016-observability-optional-external-stack.md) |
| D19 | Lifecycle | Publish/promote by digest without rebuild, approvals, verified transfers, signing/attestation checks, retirement/deprecation, audit and rollback of references. | [0003](../adr/0003-separate-build-validation-publication-lifecycles.md) |
| D20 | Automation | Git-driven build triggers and focused image pipelines, approvals, API/CLI integration with external CI; not a generic workflow engine. | — |
| D21 | Events | Pluggable in-app/email/Slack/Teams/webhooks; routing, subscriptions, retries, DLQ, test delivery and audited status. | — |
| D22 | UI | Original React design, conceptually similarly capable admin UX; do not copy prior-project components or styling assets; beginner-first guided experience. | [0006](../adr/0006-canonical-wizard.md), [0007](../adr/0007-canonical-navigation.md) |
| D23 | Install | Compose/native Linux and later Helm; first-run wizard and configuration-as-code, preflight and secure resumable bootstrap. | [0013](../adr/0013-buildkit-default-engine-and-dev-environment.md) |
| D24 | Isolation | Ephemeral per-build environments, no unsafe host socket, rootless where supported, stronger VM-based profiles for untrusted builds, CPU/memory/disk/network/time limits, job-scoped secrets and optional dedicated pools. | [0013](../adr/0013-buildkit-default-engine-and-dev-environment.md) |
| D25 | Locations/federation | Location-aware pools first, optional future authenticated controller federation with policy-scoped sharing, controlled cross-location routing, clear job ownership. | — |
| D26 | Cache | Worker-local + admin-managed shared caches; isolation and trust domains, TTL/quotas/monitoring/clean build, no secret leakage through caches. | — |
| D27 | Backup/DR | Admin-managed encrypted backup/restore, validation, external backup adapters, consistency procedures, HA, documented RPO/RTO and disaster testing. | — |
| D28 | Upgrades | Supported version matrix, worker draining/rolling upgrades, controller migration safeguards, schema evolution, compatibility checks and audited updates. | — |
| D29 | API | Versioned OpenAPI API; primary GUI, optional CLI/SDK; all channels share authorization, validation and execution paths. | [0010](../adr/0010-openapi-toolchain-and-generated-clients.md) |
| D30 | Tenancy | One organization first with strong team/project isolation, scoped infrastructure/quotas, tenant-aware model for possible future multi-organization use. | — |
| D31 | Delivery | Implement the *entire* product as one comprehensive initiative; use internal milestones, tests and release gates rather than a perpetual partial prototype. | [roadmap](../roadmap.md) |

## Wording changes from the original directive (non-semantic)

- D13: "Simple/Detailed/YAML" → "Guided/Detailed/YAML" (owner decision, ADR 0005). Same three views. Amended 2026-10-26 (owner): one Form plus YAML (ADR 0005 amendment).
- D11: original says "optional Vault PKI"; OpenBao PKI is the preferred implementation and Vault PKI remains a
  compatible option (owner decision, ADR 0015).
