# Build Cache Isolation

Status: **Authoritative** (owner decisions OD-16 to OD-19, 2026-10-19). Evidence: `docs/spikes/buildkit-secrets.md`.
Threats: TM-44, TM-87 to TM-89. Related: ADRs 0013, 0027, 0029.

## Why

BuildKit does not include secret values in cache keys. That is documented behavior, and the OD-15 spike observed
it (S8). It also merged identical concurrent steps from different sessions on one daemon (S7). Cache reuse can
therefore carry results derived from one project's secrets, or from a rotated secret, into another build. Security
takes priority over cache efficiency.

## Definitions

- A **secret-bearing build** is one whose plan lists at least one build-time secret.
- A **secret-dependent result** is any cache record produced by, or downstream of, a step that mounted a secret.
- A **cache scope** is the tuple (organization, trust domain, project, set of secret-binding version IDs, build-policy
  version). Binding version IDs are opaque identifiers assigned by Samuori when a binding is created or rotated. They are
  never derived from secret values. A `latest` binding must resolve to a concrete provider version at admission;
  otherwise the build is non-cacheable.

## Rules

1. **One build per daemon** (OD-16). Concurrency never comes from sharing a `buildkitd`.
2. **Fresh daemon per secret-bearing build** (OD-18). The daemon and its local state are discarded afterwards, so
   no local cache survives into another build.
3. **No cache reuse for secret-bearing builds until scoped reuse is enabled.** Until the controls in "Scoped reuse"
   pass in CI (OD-19):
   - secret-bearing builds run with cache import disabled;
   - they export no cache;
   - each uses an isolated local cache that is destroyed with its daemon.
4. **Never share secret-dependent results** across projects, trust domains or binding-version sets. This holds even
   after the controls are validated.
5. **Rotation and revocation invalidate.** A new binding version, or a revoked binding, makes every cache scope
   that included the old version ineligible. Such scopes are deleted by garbage collection, not reused.
6. **No secret material in keys.** Never plaintext secrets, and never hashes of secret values: low-entropy values
   such as passwords could be guessed offline. Only binding version IDs.
7. **Exports respect authorization.** Cache exports go only to internal storage locations named by the scope.
   Workers receive write access to exactly that location for one attempt (ADR 0024 storage rules). Users cannot
   point exports anywhere.
8. **Clean builds** are available to administrators and to developers authorized on the project. Every rebuild of
   a secret-bearing build is clean until scoped reuse is enabled.
9. **Secret-free builds** may use trust-domain-scoped caches (ADR 0013 rules), since no secret-dependent records
   exist.

## Scoped reuse (OD-19, approved with conditions; disabled until CI evidence)

Evidence: `docs/spikes/cache-isolation.md`. BuildKit itself enforces none of these boundaries, so Samuori must.
- The controller is the sole authority for cache scopes. It computes the scope at admission and assigns exactly one
  import location and one export location per attempt, through storage grants. Workers never choose, list or
  pattern-match cache sources and never import from anywhere external.
- Scope references are validated before import and before export: they must belong to the attempt's scope, and the
  scope's bindings must all still be active at their recorded versions.
- Rotation or revocation of any binding in a scope makes that scope ineligible immediately. Garbage collection
  removes it.
- Reuse happens only within an identical scope. There is no cross-project, cross-trust-domain, cross-version or
  partial (prefix) reuse.
- Fresh daemon per secret-bearing build (OD-18) and one build per daemon (OD-16) still apply.
- **Enablement gate:** scoped reuse stays off until the controller-side controls exist and SEC-CCH-02 to SEC-CCH-06
  pass in CI. Rule 3 above applies until then.

## Shared caches (D26)

Implemented in WP27 for secret-free builds (rule 9), with scopes still per project, which is stricter than the trust
domain. Secret-bearing builds keep rule 3: `ScopedReuseEnabled` is `false`, so they get `cache.mode: none`, and the
controller refuses them both cache imports and cache upload grants.

| Step | How |
|---|---|
| Scope | Computed at admission and stored in the immutable plan (`cache.mode: scoped`, `cache.scope`): a hash of organization, trust domain, project, binding version IDs and policy versions, never of secret values. Clean builds and unresolved `latest` bindings get `none` |
| Import | Before the build, the worker calls `GetCache` with its lease. The controller looks up the plan's scope itself and streams that scope's newest cache that came from a **succeeded** run, within the TTL and size limit. The worker cannot name a scope, a location or a digest |
| Unpack | The worker checks the stream against the announced digest and size, then unpacks it as an OCI layout: only `oci-layout`, `index.json` and `blobs/sha256/<digest>` regular files, each blob verified against its name, within the size limit. Any violation discards the whole cache, and the build runs without it |
| Use | BuildKit imports it into the fresh daemon (`cache-from type=local`) and exports this build's cache (`cache-to type=local,mode=max`). Buildah builds use no shared cache |
| Export | The provider packs the exported layout deterministically, and the worker uploads it through a grant bound to the plan's scope (SEC-CCH-04, SEC-CCH-06) |
| Retention | Garbage collection keeps only each scope's newest importable cache and drops caches older than `buildCache.ttl` (default 168 h) or larger than `buildCache.maxScopeBytes` (default 10 GiB) |
| Administration | **Infrastructure > Storage > Shared build caches** (`GET /build-caches`, administrators and operators) shows each scope's project, size, newest cache and last use. **Purge** (`POST /build-caches/{scope}/purge`, administrators) removes a scope's caches and is audited; the next build runs without cache |

Railpack builds (ADR 0047) also pass the project's scope ID as the frontend's `cache-key`, which prefixes every
Railpack cache mount; mounts live in the fresh daemon and are not exported, so they never cross builds today.

Buildpacks builds (ADR 0047) keep no cache yet, whatever the plan's cache mode: the worker imports and exports no
BuildKit cache for them (it would hold both base images and the lifecycle's layers, reusable only by an identical
rebuild) and gives the lifecycle no cache directory, so nothing crosses builds or projects (`TestBuildpacksRequest`,
SEC-CNB-07). A lifecycle cache per cache scope is future work.

A cache comes only from builds of the same project and configuration, so a poisoned cache can affect only that
project's later builds. Clean builds bypass it, and purge removes it. Image validation still checks every output.

## Implementation status (WP18)

- The controller computes each plan's cache scope at admission from organization, trust domain (`default` until
  projects map to trust domains in WP24), project, binding version IDs and policy versions, and stores it in the
  immutable plan (`cache.mode`, `cache.scope`). Scope IDs are hashes of these identifiers, never of secret values.
- `ScopedReuseEnabled` is `false`: secret-bearing builds, clean builds and builds with an unresolved binding
  version get `cache.mode: none`, and cache upload grants and imports are refused for them.
- Cache import and export for secret-free builds: see "Shared caches (D26)".

## Tests

SEC-CCH-01 to SEC-CCH-06, SEC-SEC-11 to SEC-SEC-14 (`docs/developers/security-test-catalog.md`).
