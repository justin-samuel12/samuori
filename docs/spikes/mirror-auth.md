# Spike: Private Registry Mirror Authorization (OD-14)

Date: 2026-10-19. Status: **Complete. Token authentication enforced per-project access on cached content in every
test; a decision is requested on enabling shared private caching (OD-14).** Code: `tools/spikes/mirror-auth/`.
Raw output is kept in the maintainer's private records.

## Setup (disposable VM)
- **Upstream:** CNCF Distribution `registry:3` with basic auth, holding `private/app` (busybox plus a unique
  "project 1" layer) and `private/other`.
- **Mirror:** `registry:3` in pull-through proxy mode. It holds upstream credentials (`mirror` user) and uses
  **token authentication**, trusting a signing certificate from a spike token issuer.
- **Token issuer** (`tokensrv`, standard library only): stands in for Samuori. It authenticates projects and grants
  `pull` only on repositories allowed by policy (p1 → `private/app`, p2 → `private/other`), as ES256 JWTs with an
  `x5c` header.
- All requests were raw HTTP with explicit tokens, so each result is a plain status code.

## Results

| Test | Status | Expected |
|---|---|---|
| T1 p1 pulls `private/app` manifest (fills the cache) | 200 | 200 |
| T1 p1 fetches the unique layer | 200 | 200 |
| T2 p2 requests the cached `private/app` manifest | 401 | 401 |
| T2 p2 requests the cached unique layer via `private/app` | 401 | 401 |
| T3 anonymous `private/app` manifest | 401 | 401 |
| T4 p2 pulls its authorized `private/other` | 200 | 200 |
| T5 p2 requests project 1's unique layer **through its own authorized repository** | 404 | not 2xx |
| T6 (upstream stopped, cache only) p1 cached manifest | 200 | 200 |
| T6 (upstream stopped) p2 cached `private/app` manifest | 401 | 401 |
| T6 (upstream stopped) p2 unique layer through `private/other` | 500 (upstream error) | not 2xx |
| T7 p2 `/v2/_catalog` | 401 | 401 |
| T8 p2 `tags/list` on `private/app` | 401 | 401 |

The token issuer logged exactly the expected grants and denials.

## Findings
1. Distribution's token authentication checks the token's repository scope on **every** request, including content
   already in the proxy cache. A project-scoped token is therefore enforced by the mirror itself, which is what
   ADR 0030 requires (not "tokens alone with an unchecked cache").
2. Blobs are not served across repositories by digest: a blob cached for `private/app` was not retrievable through
   `private/other`.
3. With the upstream unavailable, a non-linked blob produced 500 rather than 404. That is no leak, but Samuori should
   map mirror errors to a plain "base image source unavailable" message.
4. Catalog and tag listing are denied without explicit scopes. Samuori's issuer grants no catalog scope.
5. The mirror's upstream credential is shared by everyone. Authorization lives entirely in the Samuori token issuer's
   policy, which therefore needs the SEC-REG-04 to 07 tests.

## Decision requested (OD-14)

Recommended: **allow a shared private-image mirror per upstream credential profile, using Distribution token
authentication**, under these conditions:
- the controller issues short-lived, per-attempt tokens scoped to exactly the repositories the build's project may
  pull, and never grants catalog scope;
- each mirror instance proxies one upstream with one pull profile;
- negative tests T2, T3, T5, T7 and T8 become SEC-REG-04 and SEC-REG-07 and must pass in CI before sharing is
  enabled.

Alternative: keep one mirror instance per authorization boundary, the current rule. That is simpler to reason about
and duplicates storage.

## Limitations
- One Distribution version (`registry:3` as pulled on 2026-10-19), single instance, HTTP inside the VM.
- Not tested: token replay by another project (bounded by short expiry and audience), Harbor proxy cache, ECR pull
  through cache. Those belong to the M9 registry spikes.
