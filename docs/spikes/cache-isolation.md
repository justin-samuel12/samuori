# Spike: Secret-Aware Cache Isolation (OD-19)

Date: 2026-10-19. Status: **Complete. Scoped reuse works only when Samuori strictly controls import selection; a
decision is requested on enabling it (OD-19).** Code: `tools/spikes/buildkit-secrets/cache-scope.sh`. Raw output is kept in the
maintainer's private records.

## Setup
- BuildKit v0.34.0, rootless, in a disposable VM.
- **Every build ran on a freshly started daemon with a fresh, then deleted, state directory** (OD-18).
- Cache scopes were local cache directories named by (project, binding version), as `build-cache.md` proposes.
- Dockerfile: one secret-independent step, one step deriving a hash from secret `alpha`, one step reading it.
- Three synthetic values: project 1 binding v1, project 1 binding v2 (rotated), project 2 binding v1.

## Results

| Build | Import from | Cached `RUN` steps | Secret requested | Output derived from |
|---|---|---|---|---|
| c1 first build, scope p1-v1 | none | 0 | yes | p1v1 ✔ |
| c2 same scope again | p1-v1 | 3 | no | p1v1 ✔ |
| c3 rotated binding, scope p1-v2 | p1-v2 (empty) | 0 | yes | p1v2 ✔ |
| c4 fresh daemon, no import | none | 0 | yes | p1v1 ✔ |
| **c5 negative control:** rotated secret, imports p1-v1 | p1-v1 | 3 | **no** | **p1v1, stale** |
| **c6 negative control:** project 2, imports p1-v1 | p1-v1 | 3 | **no** | **p1v1, project 1's data** |

No exported cache file contained any secret value.

## Findings
1. A fresh daemon with no import reuses nothing, so OD-18 alone prevents cross-build reuse.
2. Reuse within an identical scope (same project, same binding versions, therefore the same secret values) is
   correct and skips the secret request entirely.
3. **BuildKit offers no protection once a cross-scope import happens.** Even the secret-dependent step is reused,
   carrying stale or another project's derived data. The boundary exists only in how Samuori selects the import and
   export location.
4. There is no generic way to reuse only the secret-independent prefix: an imported cache matches every step whose
   inputs match, and secret values are not inputs.
5. Cache exports contained no secret values; they contain derived data, which is exactly what must not cross
   scopes.

## Decision requested (OD-19)

Recommended: **enable cache reuse for secret-bearing builds only within an identical scope**, under all of these
conditions:
- The scope key is (organization, trust domain, project, the exact set of resolved binding versions for every
  secret in the plan). It contains no values and no hashes of values.
- A binding with version policy `latest` must resolve to a concrete provider version at admission (OpenBao KV v2
  version, AWS Secrets Manager `VersionId`, local store version). Without one, the build is non-cacheable.
- The controller alone chooses the import and export location. The worker receives a grant for exactly that
  location (SEC-CCH-04) and never a list or pattern.
- A fresh daemon per build still applies (OD-18). Concurrent builds never share a daemon (OD-16).
- No cross-scope import, ever. No partial or prefix reuse.
- Negative tests reproduce c5 and c6 and must fail to reuse (SEC-CCH-02); rotation must miss (SEC-CCH-05).

Alternative: keep isolated, non-reusable caches for secret-bearing builds, the current rule. That is simpler and
costs only build time for secret-bearing builds.
