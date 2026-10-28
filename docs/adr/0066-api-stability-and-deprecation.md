# 0066. API stability and deprecation

Status: **Accepted** (2026-10-28, owner direction to make the API usable from outside the repository; provisional,
may change during acceptance testing). Builds on [ADR 0010](0010-openapi-toolchain-and-generated-clients.md) and the
placeholder group of [ADR 0008](0008-api-group-placeholder.md).

## Context
The contract (`api/openapi/samuori.v1.yml`) was only in the repository. Script authors had to read the source tree to
find operations, nothing said how stable the API is, and there was no way to tell a caller that an operation is going
away.

## Options
- **Who may fetch the contract:** any signed-in user or API token (chosen: it reveals operation names, paths and the
  roles each needs, but no data, and every caller already needs to sign in to use any of it); public (rejected for
  now: it maps the attack surface for anonymous visitors and helps nobody who cannot sign in; a switch defaulting to
  off can be added later if an operator asks); administrators only (rejected: developers write the scripts).
- **Format:** the YAML exactly as built into the release, plus JSON generated from it (chosen: the YAML keeps the
  comments and ordering, JSON serves tools and the portal without a YAML parser).
- **Marking deprecation:** OpenAPI `deprecated: true` with dates as extensions, turned into headers by the request
  guard (chosen: one place, read from the same contract that routes and authorizes); a separate list in code
  (rejected: it drifts from the contract); per-handler headers (rejected: easy to forget).
- **API reference:** a portal page rendered from the served contract with the existing components (chosen); a
  third-party OpenAPI viewer (rejected: a new dependency, often loaded from a CDN, and styled outside the design
  tokens).

## Decision
- **Serving.** `GET /api/v1/openapi.yml` (`application/yaml`) and `GET /api/v1/openapi.json`, `x-samuori-roles:
  [any]`: any signed-in person or service identity, read-only tokens included; anonymous callers get 401. Responses
  carry `Cache-Control: private, max-age=300` and an `ETag`; `If-None-Match` answers 304.
- **What `v1alpha1` means.** The path `/api/v1` and group `samuori.invalid/v1alpha1` are pre-stable. Operations,
  fields and error codes can change between releases, and a consistency pass over naming and pagination is planned.
  Adding operations, optional fields and new enum values is not a breaking change; clients must ignore unknown
  fields. Every breaking change is listed in `docs/developers/api-changelog.md`.
- **Deprecating an operation.** Set `deprecated: true`, `x-samuori-deprecated: "YYYY-MM-DD"` (when it took effect)
  and, once known, `x-samuori-sunset: "YYYY-MM-DD"` (the earliest removal), and add a changelog entry naming the
  replacement. The guard then answers every call with `Deprecation: @<unix time>` (RFC 9745) and `Sunset: <HTTP
  date>` (RFC 8594). The server refuses to start, and the API tests fail, if a deprecated operation has no valid
  date, if dates appear without `deprecated: true`, or if the sunset is less than 30 days after the deprecation.
- **Notice.** While the API is `v1alpha1`, a deprecated operation stays for at least 30 days and at least one
  release that carries the headers. Removing or renaming a field or changing its meaning without an operation-level
  deprecation is allowed only as a changelog entry, because the alpha contract makes no field-level promise.
- **A stable `v1` later.** It will be declared by a new ADR that lists the operations it covers, marks them
  `x-samuori-stability: stable` in the contract and sets a longer notice (proposed: 180 days). Until then no
  operation is stable.
- **Reference page.** Help → API reference (`/help/api-reference`) lists every operation from `openapi.json`, grouped
  by tag and searchable, with roles, parameters, request and response schemas, the error format and a curl example
  for an API token. It shows the same contract to every signed-in user; the server still authorizes every call.

## Consequences
- Script authors can find and generate clients from the contract of the server they talk to.
- Deprecation needs only a contract edit and a changelog entry; callers see it in headers before removal.
- The reference page is only as good as the contract's descriptions; operations without a description show only
  their path and schemas.
