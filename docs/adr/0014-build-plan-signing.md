# 0014. Build-plan integrity and the necessity of plan signing

Status: **Deferred** (2026-10-19, owner decision OD-6). Plan signing is not implemented. This ADR records the threat
analysis that must justify it before any work starts.

## Context
The resolved build plan (`docs/architecture/buildspec.md`) is what a worker executes. An earlier draft proposed
signing every plan (Ed25519, DSSE envelope). The owner asked first to establish whether signing adds protection
beyond the controls already required:

- **Immutable plans:** stored once in the controller DB, never edited, identified by a SHA-256 content digest over
  canonical JSON.
- **Authenticated workers:** mTLS identities issued at enrollment; pinned controller identity.
- **Content digests:** the source snapshot, spec revision and base images are referenced by digest and verified
  on fetch.
- **Authorization:** the controller releases a plan only to the worker holding the current fenced lease for that
  attempt.

## Delivery path assumed by this analysis
NATS carries only wake-up notifications (run and attempt IDs). The worker fetches the plan directly from the
controller over its mTLS channel and verifies that the plan digest matches the one recorded on its lease.

## Threat analysis

| Threat | Mitigated without signing? | Notes |
|---|---|---|
| Tampered or injected message on NATS | Yes | Messages carry no plan content; workers fetch from the controller over mTLS |
| Replay of an old plan to a worker | Yes | Lease and fencing generation bind plan digest, attempt and worker |
| Network attacker between worker and controller | Yes | mTLS with pinned controller identity |
| Compromised or modified artifact storage holding plans | Yes, if plans never travel via storage | Becomes a gap if plans are ever delivered through shared storage or caches |
| Compromised controller | No, and signing would not help | The controller would hold the signing key |
| Plan forwarded between federated controllers (D25) | **Partly** | The executing worker trusts its local controller, not the originator; the originator's intent is unverifiable downstream |
| Offline or air-gapped workers receiving plans out of band | **No** | No live mTLS session to the authority that created the plan |
| Third-party verification after the fact (audit, provenance) | Partly | SLSA provenance attestations and the audited plan digest cover most needs; a signature adds non-repudiation of controller intent |

## Decision
- Do not implement plan signing now. Implement plan integrity through immutability, content digests, lease-bound
  authorization and mTLS delivery, with negative tests for each row above that is marked "Yes".
- Plans must never be delivered through NATS payloads or shared storage while signing is absent. If a design change
  requires either, this ADR must be revisited first.
- Revisit before M11 federation work or any offline-worker feature. If signing is then justified, the earlier
  proposal remains the starting point: RFC 8785 canonical JSON, DSSE envelope, Ed25519, keys behind a `PlanSigner`
  interface, trust bundle delivered to workers at enrollment.

## Consequences
One less key-management responsibility for the vertical slice. The threat-model document
(`docs/architecture/security.md`) carries these rows and their tests.
