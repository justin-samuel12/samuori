# 0030. Registry authentication: build-time pulls and controller-only publication

Status: **Accepted as direction** (2026-10-19, owner decision OD-13). Design:
[`../architecture/registry-auth.md`](../architecture/registry-auth.md). Builds on ADRs 0024 and 0025.

## Context
Builds may pull private base images and other OCI artifacts. Completed images are pushed or promoted to ECR and
other registries. These are different trust problems and must not share credentials.

## Decision
- **Credential profiles** are admin-managed records for each registry:
  - **ECR:** ambient workload identity (instance profile or IRSA-style web identity), STS AssumeRole with an
    external ID (including cross-account), or IAM Roles Anywhere.
  - **Other registries:** username/password or access tokens held as secret references.
  - Every profile has test, rotation, authorization and audit support.
- **Separate pull and push profiles.** Pull profiles serve the approved mirror. Push profiles serve publication
  only. A profile is never used for both.
- **Build-time pulls go through the approved mirror** (ADR 0025). The mirror holds upstream pull credentials.
  Workers and build steps receive only a short-lived, Samuori-issued mirror token. That token is scoped to the
  repositories the build's project is authorized to pull.
- **Publication is controller-only** (ADR 0024). Push credentials are resolved just in time on the controller,
  refreshed before expiry (ECR tokens last 12 hours), and never sent to workers.
- Developers see only the image sources and destinations they are authorized to use.
- An authentication failure is reported as such. Samuori never falls back to anonymous access or to another profile.

## Consequences
- The mirror must enforce per-project authorization for private upstream images, including for content already
  cached (threat TM-65). Project-scoped tokens are not enough on their own if the cache can return content without
  checking authorization itself. The OD-14 spike (`docs/spikes/mirror-auth.md`) showed that Distribution token
  authentication enforces repository scopes on cached manifests and blobs and denies cross-repository blob,
  catalog and tag access.
- **OD-14, approved with conditions (2026-10-19):** one shared mirror per upstream credential profile is allowed,
  with these conditions:
  - controller-issued, short-lived, build-scoped tokens;
  - authorization on every endpoint, including cached content, alternate paths and digests;
  - no upstream credentials on workers;
  - recheck after credential rotation or access revocation;
  - **fail closed** when the token or authorization service is unavailable.

  Isolated per-boundary mirrors remain in force until SEC-REG-04, SEC-REG-07 and SEC-REG-08 pass in CI.
- Registry-specific capabilities (ECR immutability, scanning, lifecycle) are surfaced, never assumed.

## Implementation (WP27, 2026-10-20)
- Authorizing mirrors use Distribution token authentication with Samuori as the only issuer. Workers obtain
  per-attempt, five-minute, repository-scoped tokens over the worker protocol and hand them to BuildKit through the
  session. ECR push profiles support workload identity, assume-role with an external ID and flagged access keys;
  IAM Roles Anywhere is not available yet. Shared mirrors stay off by default (OD-14). See `registry-auth.md`.
