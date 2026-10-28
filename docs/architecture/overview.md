# Architecture Overview

Status: **Authoritative** for structure; component choices marked *candidate* are pending the verification gates in
[`integrations.md`](integrations.md). License/version facts live in [`../dependencies.md`](../dependencies.md).

## Components

| Component | Implementation | Notes |
|---|---|---|
| Web UI | React + TypeScript (Vite), original design, React Aria Components and Tailwind CSS under an original Samuori design system (ADR 0023) | Typed client generated from OpenAPI (ADR 0010) |
| Control plane | Go HTTP API (`samuori-server`), typed domain model, OpenAPI 3 | Single authenticated API used by the web UI, optional CLI (`samuori`) and pipelines |
| Worker | Go (`samuori-worker`) driving a build engine | Separate process/user even in all-in-one (ADR 0012) |
| Metadata DB | SQLite (single controller) / PostgreSQL (multi-controller HA) | Authoritative state; transactional outbox (ADR 0017) |
| Messaging | Embedded NATS JetStream; optional external cluster | Transport only, never source of truth (ADR 0017) |
| Build engine | BuildKit default; Buildah/Podman later; Shipwright optional (Kubernetes) | ADR 0013 |

## Reference stack and integration policy

Every integration sits behind a capability-checked adapter with deterministic error semantics.

| Need | Default / candidate | Integration policy |
|---|---|---|
| Detection / build planning | Original read-only manifest inspectors (baseline); Railpack and/or Cloud Native Buildpacks as optional plan providers | Run detectors in a read-only sandbox; normalize evidence; coverage is capability-dependent. Railpack planning executes a downloaded `mise` binary and CNB detect executes buildpacks, so neither is a safe discovery baseline (see `discovery.md`) |
| Image execution | BuildKit (default); Buildah/Podman future; Shipwright optional Kubernetes backend | Pluggable engine; explicit OS/architecture support matrix; Windows needs Windows-compatible execution |
| Messaging | Embedded NATS/JetStream; external NATS optional | DB-authoritative jobs; durable wake-ups, idempotent dispatch, replay recovery |
| Metadata | SQLite / PostgreSQL | Portable migrations, indexes, transactional outbox, backup-safe schema evolution |
| Authentication | Local/bootstrap; generic OIDC and LDAP adapters; Keycloak optional (as an OIDC provider) | Never require Keycloak; RBAC evaluated server-side |
| Secrets | Encrypted local values; OpenBao (preferred external); HashiCorp Vault (optional compatibility adapter); AWS Secrets Manager | References not plaintext; just-in-time scoped release (ADR 0015) |
| Registry | OCI Distribution-compatible; ECR adapter; Harbor optional | Honor registry capabilities; ECR repo creation admin-only |
| OCI movement | ORAS / Skopeo / OCI client libraries (e.g. go-containerregistry) | Digest verification; signatures/attestations checked when copying |
| Security | Trivy/Syft (scans/SBOM), Cosign (signing/verification), optional OPA | Pluggable scanners, signing modes, policies, exceptions |
| Metrics/traces | OpenTelemetry; built-in UI dashboards | Prometheus, Grafana, Loki optional **external** integrations, never bundled (ADR 0016) |
| Backups | Database-native tools, filesystem/object-store snapshots; optional Kopia/pgBackRest | Restore tests; cross-store consistency; external managed systems are not magically backed up |
| Distribution | Docker Compose and native Linux (systemd) first; Helm later | Same release, role flags (ADR 0012) |

Do not embed Harbor, Keycloak, Grafana, Kubernetes or Backstage as hard dependencies. Shipwright requires Kubernetes
and is an optional adapter, not the default worker. Do not adopt provider-specific capabilities as unconditional
promises. Avoid importing vendor SDKs into domain models.

## Operational topology matrix

| Topology | Controllers | DB | Messaging | Workers | Status |
|---|---|---|---|---|---|
| All-in-one (Compose or native Linux) | 1 | SQLite | Embedded NATS | 1 local worker process; `vm-per-job` with KVM, or `rootless-dedicated-vm` on a dedicated host (ADR 0027); local mirror and egress proxy | Primary target for vertical slice |
| Single controller + remote workers | 1 | SQLite or PostgreSQL | Embedded NATS | N enrolled remote workers | M7 |
| HA controllers | ≥2 | PostgreSQL (required) | External clustered NATS | N | M11 |
| Federated locations | Per-location controllers | Per controller | Per controller | Location pools | M11 (optional, D25) |
| Kubernetes (Helm, Shipwright optional) | ≥1 | PostgreSQL | External NATS | Shipwright or worker pods | M11 (Helm, D23); Shipwright adapter M9 |

Tested OS and architecture combinations are recorded in `docs/operators/all-in-one.md` (Evidence) and
`docs/release-readiness.md`; a dedicated support matrix is not written yet. Unsupported combinations fail **before scheduling**.

## Data flow (summary)

Browser → `samuori-server` (authn/z, validation, policy) → DB transaction (state + outbox) → NATS wake-up →
`samuori-worker` claims a fenced lease → fetches its assigned source snapshot and immutable build plan (digest-verified, ADR 0014) → BuildKit builds
in an isolated environment → worker uploads artifacts to designated storage → controller records results →
ValidationRun → Publication to an OCI registry. Detailed diagrams: [`security.md`](security.md) (trust zones),
[`scheduler-and-workers.md`](scheduler-and-workers.md), [`build-lifecycle.md`](build-lifecycle.md).
