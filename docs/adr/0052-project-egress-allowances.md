# 0052. Project egress allowances

Status: **Accepted** (2026-10-26, owner decision; provisional, may change during acceptance testing). Extends
[ADR 0025](0025-build-network-egress.md) and [ADR 0050](0050-build-readiness-prediction.md).

## Context
Build readiness (ADR 0050) and a build's blocked-destination report tell developers when a build needs network
access only an organization administrator can open: for example the npm preset for Dockerfile builds, or
`github.com:443`, `release-assets.githubusercontent.com:443` and `objects.githubusercontent.com:443` for sharp 0.32's
libvips download. Until now the only fix was an organization-wide policy change, which the developer could not
request from Samuori and which opened the access for every project.

## Options
- **A. Organization-wide requests (rejected).** Approving would edit the egress policy, so one project's need widens
  every project's builds.
- **B. Per-project allowances (chosen, owner decision).** An approval opens the requested presets or destinations for
  the requesting project and build method only.
- **C. No request flow (rejected).** Developers keep asking administrators outside Samuori, with no evidence or audit.

## Decision
- A **project egress allowance** opens one egress **preset** or one public **destination** (`host:port`) for one
  project's builds of the listed **build methods** (by default the method of the request's evidence). It records the
  reason (the request's justification), the source request, who requested and who approved it, when, and an optional
  expiry. Status is active or revoked; an allowance past its expiry is expired.
- **Effective egress** for a build is the organization policy for its method plus the project's active allowances for
  that method. Admission, readiness, the build plan and denial advice all use it. Allowed presets add their
  destinations, even when the organization policy does not allow the preset; allowed destinations are added as
  public (non-internal) rules. The plan's egress lists the allowance IDs it includes (`egress.allowances`), so the
  plan digest covers them. Plans of projects without allowances are unchanged.
- **Never automatic.** Allowances exist only through an **egress access request** (approval kind
  `egress-access-request`) that an organization administrator other than the requester approves under Approvals.
  Project-scoped administrators cannot decide it. Rejection records the reason. A pending request for the same
  project, method and entries refuses a duplicate. Requests lapse after 14 days.
- **Evidence is server-side.** A request names where its evidence is, never its text: a build (the destinations its
  egress proxy reported denied, stored from the worker's structured report), or a readiness item (code, path and
  line) of a discovery or an admission check that the server recomputes. Every requested entry must appear in that
  evidence as requestable; the stored evidence (item message or denied destinations) comes from the server. Only
  the justification is client text.
- **What can be requested.** Built-in presets and public destinations only. Loopback, link-local, metadata,
  multicast and unspecified addresses are refused on principle, as the proxy refuses them. Private or reserved
  addresses and non-public names (single-label names, `.internal`, `.local`, `.lan`, `.corp` and similar) cannot be
  requested; an administrator adds them as internal destinations in the egress policy. Samuori still offers no
  suggestion for non-public addresses a build tried to reach.
- **Roles.** Project developers and maintainers request; project members list requests and allowances; organization
  administrators decide and **revoke** (with a reason). Every request, decision, allowance creation and revocation is
  audited.
- **Revocation and expiry** stop new plans from including the allowance. A run already queued keeps the plan it was
  admitted with, as for any policy change.
- **Advice.** Readiness and denial hints that say an organization administrator can open something add that a
  project developer can instead request access for the project only.

## Consequences
- An approval widens one project's egress, never the organization's; the threat model records it.
- The organization egress policy stays the single organization-wide document; allowances are shown on the project
  page, not in the policy editor.
- Reversible: removing the feature means dropping the allowances table and the approval kind; plans without
  allowances are byte-identical to before.

## Amendment (2026-10-27, owner decision: internal requests)
The owner decided that project members may also request **internal** destinations, with the approval as the control,
and kept the evidence rule.
- **What can be requested** now includes private addresses and non-public names, as internal destinations
  (`internalDestinations`). Each requested destination is public or internal: a public one must pass the public-host
  check and is a public plan rule; an internal one is a plan rule that may resolve to private addresses. Loopback,
  link-local (including metadata), multicast and unspecified addresses stay refused for everyone. Every entry must
  still appear in the server-side evidence; nothing can be typed in freely.
- **Choice.** The request form has a Public internet / Internal network choice per destination. Private addresses and
  non-public names can only be internal; a public name may be requested as internal, which the approver sees.
- **Approval.** Internal entries are marked `INTERNAL NETWORK` in the approval summary, and the approval page warns
  that the project's builds would reach hosts inside the network. Allowances record `internal` (migration 00062) and
  show **Internal network** on the project page.
- **Advice.** Denials of private addresses, internal names, or public names that resolved to a private address are
  now requestable as internal, and hints say an administrator can add them as internal destinations.
- **Organization policy.** Administrators add public and internal destinations separately on the Network egress page;
  a policy destination's reason is optional (policy versions are audited), while a member's justification stays
  required.

## Amendment (2026-10-27, owner decision: promoting allowances)
When several projects hold allowances for the same destination, an organization administrator can move them into the
organization egress policy.
- **Destinations only.** A preset allowed in the organization policy applies only to builds that ask for it, so a
  project's preset allowance has no faithful organization-wide equivalent; preset allowances are listed, not promoted.
- **Scope.** The policy entry opens the destination, with the allowances' network (public or internal), for every
  build method, optionally not for Dockerfile builds. The dialog says that this widens, for example, recipe-only
  allowances to all builds.
- **Replacement.** The allowances for that destination and network are revoked in the same transaction as the new
  policy version, with the reason "Moved into the organization egress policy, version N: <reason>".
- **Who.** Any organization administrator, the same right as editing the policy; no second approval. The policy
  version, each revocation and an `egress.allowance.promote` event are audited. A policy entry for the destination
  that is less open (disabled, closed to Dockerfile builds when the promotion is not, or public when the allowances
  are internal) is refused as a conflict to resolve in the policy.
