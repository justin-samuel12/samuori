# 0037. Controller federation

Status: Accepted (2026-10-20). Implements D25 within the approved scope: authenticated controller federation with
policy-scoped sharing, controlled cross-location routing and clear job ownership (`docs/architecture/scheduler-and-workers.md`).

## Context
Two independent installations, each with its own database, CA, policies and workers, should be able to run one
another's builds in agreed locations, without either trusting the other's internal state and without a build ever
having two executors.

## Decision
- **Peers.** Administrators of both installations register each other: name, endpoint, the peer's CA pin and its
  organization ID. Calls go over mutual TLS with a federation certificate issued by each installation's own CA
  (organization ID in a URI SAN) and verified against the pinned peer CA; nothing is trusted from system roots.
  Federation has its own listener (`federation.listen`), off by default.
- **Policy-scoped sharing.** The executing installation grants a peer a hosting project, the locations it may use and
  a limit on active builds. The originating installation maps a local location name to a peer location (a route);
  projects use it like any location, under the same residency rules.
- **What travels.** The originator sends the spec document and the snapshot's files, never a build plan (ADR 0014):
  the executor ingests the files under its own upload rules, runs its own admission and policies, writes its own
  plan and builds on its own verified workers, so OD-6 is not triggered. Secret-bearing builds never leave their
  installation.
- **Results.** When the executor's run and validation succeed, the originator fetches the image by digest, stores it,
  records the executor's worker checks as peer-reported and runs its own validation under its own policies before
  the run counts as succeeded and can be published. A failed or blocked run at the executor fails the originator's
  run with the executor's reason.
- **Ownership.** The originator owns its run; once the executor accepts a delegation, it alone executes it. Every call
  is made by the originator and is idempotent (the delegation ID). If the executor becomes unreachable, the run stays
  delegated, showing since when; it is never executed elsewhere. Only an administrator can abandon a delegation: the
  run then fails, a cancel is sent whenever the executor is reachable, and any later result is refused.
- **Duties** run on the leader of the originating installation.

## Consequences
- Plan signing stays unnecessary; if plans ever travel between installations, OD-6 and ADR 0014 must be revisited.
- A CA rotation at a peer requires updating its pin; until then calls fail with a clear error.
- The originator trusts runtime-test results reported by an authenticated, granted peer for builds it delegated there;
  all controller-side checks are repeated locally.
