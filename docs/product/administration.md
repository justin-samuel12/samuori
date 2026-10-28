# Administration Console

Status: **Authoritative.** This is the single canonical administration navigation (ADR 0007). Implement an original
UI; do not copy prior-project components or styling.

## Canonical navigation

Sections appear only to users whose roles grant access; the API enforces every permission regardless.

| Group | Section | Covers | Primary roles | Decisions |
|---|---|---|---|---|
| — | **Overview** | System health (from real checks only), queue depth, failing integrations, pending admin actions | Administrator, Operator | D18 |
| Organization | **Teams & Projects** | Organization, teams, projects, membership, quotas, project policies | Administrator, Maintainer | D30 |
| Organization | **Users & Identity** | Local users, LDAP/AD, OIDC, group mapping, roles/RBAC, service identities, recovery access | Administrator | D09 |
| Build infrastructure | **Build Queue** | Queued/running work, priorities, fairness, cancel/requeue | Operator | D12 |
| Build infrastructure | **Servers** | One list of controllers and workers: controller join requests (accept or reject), join codes for a new controller or worker, and removal of either (ADR 0045) | Administrator | D06, D11 |
| Build infrastructure | **Nodes & Workers** | Controllers and workers, enrollment tokens, certificates, cordon/drain/quarantine/revoke, capabilities | Operator | D06, D11 |
| Build infrastructure | **Worker Pools & Locations** | Pools, locations, data-residency rules, controller peers (federation) | Operator, Administrator | D25 |
| Build infrastructure | **Build Engines** | Engine profiles (BuildKit, later Buildah/Podman/Shipwright), OS/arch support matrix, isolation profiles | Operator | D05, D24 |
| Build infrastructure | **Recipes & Base Images** | Recipe versions, approval, deprecation, reusable steps, base-image policies | Administrator | D16 |
| Build infrastructure | **Build Cache** | Worker-local and shared caches, trust domains, TTL/quotas, purge | Operator | D26 |
| Artifacts & registries | **Registries & ECR** | OCI registries; multiple AWS accounts and regions; IAM roles, cross-account AssumeRole and Roles Anywhere; connection and optional publication tests; credential profiles (pull and push kept separate), approved image sources, approved mirrors, restricted-image destinations, repository discovery/creation, user access, repo requests (`architecture/registry-auth.md`) | Administrator | D03 |
| Artifacts & registries | **Artifact Storage** | Storage profiles (local/NFS/EFS/S3/S3-compatible/OCI), routing policies, retention, capacity | Administrator | D04, D17 |
| Security | **Policies** | Validation gates, security gates, tag rules, publication/approval rules, approved Dockerfile frontends, sensitive-output rules and exceptions, policy evaluator | Administrator, Security Reviewer | D01, D02, D14, D15 |
| Security | **Credentials & Secrets** | Secret providers (local encrypted, OpenBao, Vault-compatible, AWS Secrets Manager); project-scoped bindings (create, approve, rotate, disable); secret usage policies; secret-access audit (`architecture/secrets.md`) | Administrator | D10 |
| Security | **Certificates & TLS** | Controller TLS, worker mTLS CA, rotation, trust bundles | Administrator | D11 |
| Platform | **Database** | SQLite/PostgreSQL status, migrations, SQLite→PostgreSQL migration | Administrator | D07 |
| Platform | **Messaging** | Embedded or external NATS/JetStream status and configuration | Administrator, Operator | D08 |
| Platform | **Notifications** | Channels (in-app, email, Slack, Teams, webhooks), routing, subscriptions, DLQ, test delivery | Administrator | D21 |
| Platform | **Observability & Audit** | Metrics, traces, audit search/export, retention, optional external exporters | Administrator, Security Reviewer | D18 |
| Platform | **Backup & Restore** | Backup schedules, encryption, restore validation, external adapters | Administrator | D27 |
| Platform | **Upgrades & Diagnostics** | Version matrix, compatibility checks, worker drain/rolling upgrade, diagnostic bundles (secrets redacted) | Administrator | D28 |

Security findings and exception *approvals* for Security Reviewers live in the primary **Approvals** screen; the
policies that govern them live under Security → Policies.

Mapping from the two superseded section lists is recorded in ADR 0007.

## Rules for every connection / setting

- Editable profile, **Test** action using real connectivity, error details, health status, permissions, audit trail.
- Validate → test → save → rollback; every change audited with actor and reason.
- Dangerous changes require a dry-run / impact preview and explicit confirmation.
- Only implement admin screens backed by real handlers. No mock health, no placeholder success.
- Optional services are discoverable and recoverable without being mandatory.
- Developer views never expose secrets, cross-team metadata or unauthorized destinations.

## First-run setup

Secure, resumable and role-aware:

1. Node role: all-in-one / controller / worker.
2. Database: SQLite (default) or PostgreSQL.
3. Messaging: embedded NATS (default) or external NATS.
4. Administrator bootstrap (one-time bootstrap credential; never logged).
5. TLS.
6. Artifact storage.
7. Build engine / local worker (BuildKit preflight — see ADR 0013).
8. Optional providers (identity, secrets, registries).
9. Preflight checks.
10. Activation.

**Configuration-as-code** is also supported. Precedence (highest first): command-line flags → environment
variables → configuration file → values stored in the database by the admin UI → built-in defaults. Settings that are
set by a higher-precedence source are shown read-only in the UI with their source. Diagnostics redact secrets.
Unsafe production bootstrap defaults (default passwords, plaintext HTTP on non-loopback, disabled auth) are
rejected.
