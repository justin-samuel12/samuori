# API changelog

Changes to `/api/v1` that callers need to know about, newest first: deprecations, removals and other breaking changes,
plus notable additions. Rules: [ADR 0066](../adr/0066-api-stability-and-deprecation.md). The API is `v1alpha1`;
nothing is stable yet.

## 2026-10-28

- Added `POST /auth/session/renewal` (`renewSession`, additive): re-authenticates the user of a browser session and
  replaces it with a new session (ADR 0068). `GET /auth/session` and the renewal response report `renewWith` and
  `providerName`; `GET /auth/oidc/{providerId}/start` takes an optional same-origin `returnTo`, and the OIDC
  callback now redirects there instead of always to `/`.
- Added `GET /openapi.yml` and `GET /openapi.json` (`getApiContract`, `getApiContractJson`): the contract the server
  runs, for any signed-in caller.
- Added deprecation signalling: operations marked `deprecated: true` answer with `Deprecation` and `Sunset` headers.
- Added `GET /snapshots/{snapshotId}/spec-suggestions` (`listSpecSuggestions`, developer and maintainer): starting
  BuildSpecs derived from the snapshot's newest completed discovery, including built-in recipe specs. Additive.
- Every list now takes `limit` and `cursor` and returns `nextCursor` (50 more lists), except nine bounded ones
  (sign-in providers, build attempts and lineage, help topics, provider catalogues, conversion findings, a registry's
  repositories). Lists that used to return everything still do when called without `limit` or `cursor`; pass `limit`
  to page. An unknown cursor answers `400 invalid_cursor`.
- `listRollouts` takes the shared `limit` (1–200, default 50; was 1–100, default 20) and `cursor`. Lists read from the
  database now order by creation time and then ID, so pages are stable.
- Every create (`POST` answering `201`) accepts `Idempotency-Key` (36 more operations, optional on them). A repeat
  with the same key does not create again and answers `409 idempotent_replay` naming the first resource in
  `details`; a key whose request was refused with a 4xx can be reused.
- Every error response is documented with the shared `Problem` schema: `400`, `401`, `403` and `429` on every
  operation that can return them, and `409` on two deletes that described it without a body.
- Every `429` declares `Retry-After`; the federation executor's `429` now sends it too.
- No operation is deprecated.
