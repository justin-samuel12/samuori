# Administration Console

Status: **Authoritative.** This is the single canonical administration navigation (ADR 0007). Implement an original
UI; do not copy prior-project components or styling.

## Canonical navigation

Sections appear only to users whose roles grant access; the API enforces every permission regardless.

| Section | Page | Covers | Primary roles | Decisions |
|---|---|---|---|---|
| Organization | **Health** | System health (from real checks only), queue depth, failing integrations, pending admin actions | Administrator, Operator | D18 |
| Organization, Projects | **Teams**, **Projects** | Organization, teams, projects, membership, quotas, project policies | Administrator, Maintainer | D30 |
| Organization | **Users**, **Identity & sign-in** | Local users, LDAP/AD, OIDC, group mapping, roles/RBAC, service identities, recovery access | Administrator | D09 |
| Infrastructure | **Build queue** | Queued/running work, priorities, fairness, cancel/requeue | Operator | D12 |
| Infrastructure | **Servers** | One list of controllers and workers: controller join requests (accept or reject), join codes for a new controller or worker, and removal of either (ADR 0045) | Administrator | D06, D11 |
| Infrastructure | **Workers & pools** | Controllers and workers, enrollment tokens, certificates, cordon/drain/quarantine/revoke, capabilities | Operator | D06, D11 |
| Infrastructure, Organization | **Workers & pools**, **Federation** | Pools, locations, data-residency rules, controller peers (federation) | Operator, Administrator | D25 |
| Infrastructure | **Build engines** | Engine profiles (BuildKit, later Buildah/Podman/Shipwright), OS/arch support matrix, isolation profiles | Operator | D05, D24 |
| Build supply | **Recipes & runtimes** | Recipe versions, approval, deprecation, reusable steps, base-image policies | Administrator | D16 |
| Infrastructure | **Storage** | Worker-local and shared caches, trust domains, TTL/quotas, purge | Operator | D26 |
| Build supply | **Registries & destinations** | OCI registries; multiple AWS accounts and regions; IAM roles, cross-account AssumeRole and Roles Anywhere; connection and optional publication tests; credential profiles (pull and push kept separate), approved image sources, approved mirrors, restricted-image destinations, repository discovery/creation, user access, repo requests (`architecture/registry-auth.md`) | Administrator | D03 |
| Infrastructure | **Storage** | Storage profiles (local/NFS/EFS/S3/S3-compatible/OCI), routing policies, retention, capacity | Administrator | D04, D17 |
| Build supply, Security | **Build policies**, **Egress** | Validation gates, security gates, tag rules, publication/approval rules, approved Dockerfile frontends, sensitive-output rules and exceptions, policy evaluator | Administrator, Security Reviewer | D01, D02, D14, D15 |
| Security | **Secrets** | Secret providers (local encrypted, OpenBao, Vault-compatible, AWS Secrets Manager); project-scoped bindings (create, approve, rotate, disable); secret usage policies; secret-access audit (`architecture/secrets.md`) | Administrator | D10 |
| Security | **Certificates** | Controller TLS, worker mTLS CA, rotation, trust bundles | Administrator | D11 |
| Infrastructure | **Database** | SQLite/PostgreSQL status, migrations, SQLite→PostgreSQL migration | Administrator | D07 |
| Infrastructure | **Messaging** | Embedded or external NATS/JetStream status and configuration | Administrator, Operator | D08 |
| Organization | **Notification channels** | Channels (in-app, email, Slack, Teams, webhooks), routing, subscriptions, DLQ, test delivery | Administrator | D21 |
| Security | **Audit & observability** | Metrics, traces, audit search/export, retention, optional external exporters | Administrator, Security Reviewer | D18 |
| Organization | **Backups** | Backup schedules, encryption, restore validation, external adapters | Administrator | D27 |
| Organization | **Upgrades & diagnostics** | Version matrix, compatibility checks, worker drain/rolling upgrade, diagnostic bundles (secrets redacted) | Administrator | D28 |

Security findings and exception *approvals* for Security Reviewers live in **Builds > Approvals**; the
policies that govern them live under Build supply > Build policies.

The table names each area by where it is in the portal (ADR 0070): four top-bar sections, **Build supply**,
**Infrastructure**, **Security** and **Organization**, and the page inside it. **Health** (`/admin/health`) is the
administration overview; `/admin` redirects to it. Guides and documents name a page as *Section > Page*, for example
Infrastructure > Workers & pools. The catalogue of areas and their mapping from the superseded lists is in ADR 0007.

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
