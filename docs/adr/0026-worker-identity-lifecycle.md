# 0026. Worker and controller identity lifecycle

Status: **Accepted as direction** (2026-10-19, owner decision OD-11). Threat rows: TM-30 to TM-34 in `docs/architecture/threat-model.md`.

## Context
Workers pin the controller's identity at enrollment (D11). Pinning a single leaf certificate would make routine
certificate rotation, host renames and restores into outages that require re-enrolling every worker.

## Decision

### Trust anchors
- Workers pin the **Samuori internal CA** (by SPKI hash), not a controller leaf certificate. The pin is delivered with
  the enrollment token.
- The controller's TLS serving certificate is issued by that CA, is short-lived and rotates automatically. Workers
  accept any valid serving certificate chaining to a pinned CA, so hostname, IP or certificate changes need no
  worker action.
- Optional external PKI (OpenBao PKI, or Vault PKI through the compatibility adapter) can replace the internal
  CA. The worker still pins a CA, never a leaf.

### Worker certificates
- Lifetime: 7 days by default, renewed at two-thirds of its lifetime over the existing mTLS channel with a fresh
  key pair (new CSR each time).
- The private key is generated on the worker and never leaves it.

### Trust bundle and CA rotation
- Workers hold a **trust bundle** (one or more CA public keys with validity windows), refreshed from the controller
  over mTLS on every connection.
- Planned CA rotation: publish the new CA in the bundle → wait until every active worker has acknowledged it →
  switch issuance to the new CA → retire the old CA after the longest worker-certificate lifetime has passed.
  Workers that have not acknowledged block the switch and are listed for the operator.

### Revocation
- Revocation takes effect immediately at the controller: the identity is denied on every request and open
  sessions are closed. Short certificate lifetimes bound the exposure if the controller's deny list is lost in a
  restore.
- Leases held by a revoked worker are fenced and its attempts requeued.
- When a worker is revoked for suspected compromise, artifacts it produced since its last known-good time are
  flagged and their publication is blocked pending review (threat-model worker-compromise section).

### Recovery scenarios

| Scenario | Behavior | Operator action |
|---|---|---|
| Worker offline longer than its certificate lifetime | Renewal fails; worker reports "expired identity" | Issue a new enrollment token; the old identity is revoked automatically on re-enrollment |
| Controller host renamed, re-addressed or restored from backup | Workers keep working; the pin is the CA | None if the CA key was restored; otherwise see "CA key lost" |
| Clock skew on a worker | Renewal and TLS fail with a skew diagnostic | Fix time sync; preflight checks NTP |
| CA key lost (no backup) | Workers can no longer renew | Create a new CA, then re-enroll all workers (runbook). CA keys are therefore included in encrypted backups (D27) or held in external PKI |
| CA key suspected compromised | Emergency rotation | New CA, revoke all worker identities, re-enroll every worker out of band. A bundle signed by the compromised CA is not trusted for the switch |
| Enrollment token leaked before use | Single-use, short TTL | Revoke the token; it was never logged |

## Implementation (2026-10-19, WP24)
Implemented as decided, with these details: the previous certificate stays valid only until its replacement is first
used; workers acknowledge bundle versions in Hello and heartbeats; the old CA retires after the worker certificate
lifetime plus the serving certificate lifetime; revocation hooks fence leases and quarantine a compromised worker's
images, which publication refuses until a reviewer releases them. External PKI (OpenBao, Vault) remains WP27.
Runbook: `docs/operators/worker-identity.md`.

## Consequences
- Requires runbooks for CA rotation, emergency rotation and mass re-enrollment (`docs/operators/`, written in M7).
- CA private key custody becomes part of backup scope and the threat model.
