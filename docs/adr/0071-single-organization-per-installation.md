# 0071. Single organization per installation

Status: **Accepted** (2026-10-29, owner decision that Samuori serves one customer per installation; provisional, may
change during acceptance testing). Amends decision D30 (`../traceability.md`) and the tenancy note in
[`../architecture/domain-model.md`](../architecture/domain-model.md).

## Context
The data model is tenant-shaped: every row carries an `org_id`, and the repository layer scopes every read to the
caller's organization, returning 404 on a cross-scope read (D30, `internal/tenancy`). But the running product only
ever has one organization: `CreateOrganization` is called by first-run setup and tests, and nothing else. There is no
API operation, CLI command or admin page to create, list or switch organizations.

This left the product's intent ambiguous. A reader could take the `org_id` columns as a half-finished multi-tenant
feature and either try to "finish" it or try to tear it out.

## Decision
- **One installation serves one organization.** An installation belongs to a single customer. The organization is
  created at first-run setup and is the only one. Samuori exposes no way to create, list, rename away or switch
  between organizations. Teams are that customer's internal groups; they are the isolation boundary for projects, not
  tenants.
- **The `org_id` scope stays in the data model.** It is an isolation primitive, not a disabled feature. It keeps
  every query scoped, backs clean 404s on cross-scope reads, and is already built and tested. It is not removed.
  Removing it would touch every table, query and migration to delete a working safety boundary, for no gain.
- **Multi-organization management is deliberately not built.** No "add organization" surface is added. If a future
  requirement genuinely needs several independent tenants in one installation, it is a new, owner-approved decision
  with its own ADR, threat model and tests; the schema not needing to be reshaped is the only thing kept in reserve.

## Consequences
- The product is single-tenant and simpler to secure, operate and support; a whole class of cross-tenant data-leak
  risk does not arise because there is only one tenant.
- Features scoped "per organization" (for example installation-wide settings and branding, ADR 0072) are effectively
  per-installation. Keying their records by `org_id` is cheap future-proofing, not access to a multi-org feature.
- D30 and the domain-model tenancy note are updated to say one organization is the supported and intended state, not
  merely "supported first".
- Server-side authorization is unchanged. Team isolation and the SEC-RBAC guarantees continue to rely on the same
  scoping.
