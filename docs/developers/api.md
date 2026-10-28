# Using the Samuori API

A short guide for scripts and integrations. It describes the API as it behaves today; design rules are in
[architecture/api.md](../architecture/api.md) and stability rules in
[ADR 0066](../adr/0066-api-stability-and-deprecation.md). A consistency pass over naming and pagination is planned,
so check the [API changelog](api-changelog.md) when you upgrade.

## The contract

Every server publishes the contract it runs at `GET /api/v1/openapi.yml` and `GET /api/v1/openapi.json`. Both need a
signed-in session or an API token, carry an `ETag` and may be cached privately for five minutes. In the portal,
**Help → API reference** shows the same contract with search, schemas and curl examples. Generate a client from it
with any OpenAPI 3.0 generator; Samuori itself uses oapi-codegen and openapi-typescript (ADR 0010).

## Authentication

- **API tokens.** Create one on **Account → API tokens** (`POST /me/api-tokens`): a name, an expiry and `read` or
  `read` plus `write`. The value is shown once. Send it as `Authorization: Bearer <token>`. A token acts with its
  owner's roles; a read-only token is refused for anything but `GET` and `HEAD` (`403 read_only_token`).
- **Service identities.** For automation, an administrator creates a service identity, grants it roles and issues
  it tokens (`/service-identities`, [identity](../architecture/identity.md#service-identities)). Disabling the
  identity stops its tokens at once.
- **Browser sessions.** The portal uses the `samuori_session` cookie. With a session, every request other than `GET`
  and `HEAD` must also send `X-CSRF-Token` with the value of the `samuori_csrf` cookie, or it is refused with
  `403 csrf`. Bearer tokens never need it.
- Worker identities are never accepted here; workers use their own protocol (ADR 0024).

Every operation lists the roles that may call it in `x-samuori-roles` (`any` means any signed-in caller). That check
is coarse: the server also checks the organization, team or project the call names, and answers 404 for resources
outside the caller's scope.

```sh
curl -H "Authorization: Bearer $SAMUORI_TOKEN" https://samuori.example.com/api/v1/me
```

## Requests and responses

- **Errors** are JSON `Problem` objects: `code` (stable, for matching), `message` (for people), optional `details`
  and `requestId`. Quote the request ID to an operator; the same ID is in the `X-Request-ID` response header.
- **Pagination.** Lists that can grow take `limit` (1–200, default 50) and `cursor`, and return `nextCursor` while
  more remain; pass it back as `cursor`. Not every list is paginated yet; those return all items.
- **Changes to specs and policies** need `If-Match` with the `ETag` you read (412 on a mismatch).
- **Idempotency.** Starting builds, cancelling them, publishing, deciding approvals, committing uploads and a few
  other state changes require an `Idempotency-Key` header; repeating a request with the same key does not repeat the
  change. The contract marks each one.
- **Asynchronous work** answers `202` with an `Operation` to poll at `/operations/{id}`.
- **Body size.** Request bodies are limited to 1 MiB unless an operation declares another limit
  (`x-samuori-max-body-bytes`), such as upload chunks and update bundles.
- **Version.** Authenticated responses carry `X-Samuori-Version`, the server's release.

## Rate limits

Each signed-in caller has a token bucket of 20 requests per second with bursts of up to 100. Above it the answer is
`429 rate_limited` with `Retry-After` in seconds. Sign-in, pipeline webhooks and cluster joins have their own,
stricter limits.

## Deprecation

A deprecated operation is marked `deprecated: true` in the contract and answers with a `Deprecation` header (RFC
9745) and, once a removal date is set, a `Sunset` header (RFC 8594). While the API is `v1alpha1` it is kept for at
least 30 days and one release after deprecation. Watch for these headers in your client logs.
