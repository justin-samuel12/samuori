# API changelog

Changes to `/api/v1` that callers need to know about, newest first: deprecations, removals and other breaking changes,
plus notable additions. Rules: [ADR 0066](../adr/0066-api-stability-and-deprecation.md). The API is `v1alpha1`;
nothing is stable yet.

## 2026-10-28

- Added `GET /openapi.yml` and `GET /openapi.json` (`getApiContract`, `getApiContractJson`): the contract the server
  runs, for any signed-in caller.
- Added deprecation signalling: operations marked `deprecated: true` answer with `Deprecation` and `Sunset` headers.
- No operation is deprecated.
