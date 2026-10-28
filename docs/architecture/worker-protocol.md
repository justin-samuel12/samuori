# Worker Execution Protocol

Status: **M0 contract** (2026-10-19). Contract: `api/proto/samuori/worker/v1/worker.proto`. Decided: OD-20 (gRPC over
mTLS). Related: ADRs 0012, 0014, 0024, 0026, 0029; [`scheduler-and-workers.md`](scheduler-and-workers.md).

## Implementation status (WP12, WP24)

| Part | State |
|---|---|
| Internal CA (`internal/pki`, `<dataDir>/pki`) | An authority of ECDSA P-256 CAs (`authority.json`): one issuing, an optional next CA, retiring CAs. The worker endpoint's certificate lasts 7 days, comes from the issuing CA and is chained with it; it is reissued a day before expiry and when issuance switches. Client certificates are verified against every CA in the bundle. Worker certificates carry `urn:samuori:worker:<id>` and client-auth usage only. The controller ignores the CSR's subject |
| `Enroll` | Single-use, expiring, pool-bound tokens stored as hashes. The token is consumed and the worker recorded in one transaction. New workers are never eligible for untrusted builds |
| CA pinning | `samuori-worker --ca-pin sha256:<SPKI hex>` (shown with each token). First contact accepts only a chain whose CA matches the pin and verifies to it; afterwards the worker trusts only the returned bundle |
| `Connect` | Hello (major version 1, trust-bundle version held); HelloAck with heartbeat interval, abandoned leases, the current trust bundle and the controller's time (skew diagnostics). Heartbeats acknowledge the bundle version. The controller pushes a new bundle to connected workers when it changes |
| Identity checks | Every `WorkerService` call needs a verified certificate whose serial matches the enrolled worker (or its previous serial, until the renewed certificate is first used). `/api/v1` ignores client certificates entirely |
| `RenewCertificate` | Fresh key pair and CSR at two-thirds of the 7-day lifetime; records the acknowledged bundle version. Staged key files make an interrupted renewal recoverable |
| Revocation | Immediate: the identity is denied, the open stream closed, leases fenced and runs requeued (`FenceWorker`); for a compromised worker, its images since a given time are quarantined. Revoked and expired workers exit with status 3 |
| Re-enrollment | `EnrollRequest.previous_worker_id` revokes the worker's previous identity in the same pool |
| CA rotation | Planned: next CA in the bundle, switch only after every active worker acknowledged it, old CA retired after the longest certificate lifetime. Emergency: every CA replaced and every identity revoked ([runbook](../operators/worker-identity.md)) |
| `GetPlan`, `ReportAttempt` / `ReleaseSecret` / `GrantArtifactUpload` | `Unimplemented` until WP17 / WP16 / WP18 |

`samuori-worker` has no database access: its import graph excludes the database, tenancy, auth and API packages
(checked by a test). It reports no engines, isolation profiles or daemon slots until WP13 and WP14 provide them.

## Shape

- Workers dial out to the controller. They have no inbound ports, and they never connect to NATS.
- `EnrollmentService.Enroll` runs over TLS that verifies the pinned Samuori CA. The worker sends its single-use token
  and a CSR, and receives a short-lived certificate and the trust bundle.
- Every `WorkerService` call requires the worker's mTLS certificate. Worker identities are rejected by `/api/v1`.

| RPC | Purpose | Lease-bound |
|---|---|---|
| `Connect` (bidirectional stream) | Hello and capabilities, heartbeats, assignments, cancellation, lease renewal, drain, trust-bundle updates | Per message |
| `RenewCertificate` | New certificate from a fresh CSR; acknowledges the trust-bundle version | No (certificate-bound) |
| `GetPlan` | Immutable plan bytes, verified against `plan_digest` from the assignment | Yes |
| `ReleaseSecret` | One logical secret per BuildKit request, after lease and policy checks | Yes |
| `ReportAttempt` | State transitions, failure class, artifact digest, cleanup outcome | Yes |
| `GrantArtifactUpload` | Short-lived, object-scoped upload grant for internal storage; cache uploads only into the plan's scope | Yes |
| `GetSource` | The attempt's snapshot manifest, then each file, verified by the worker | Yes |
| `IssueMirrorToken` | A build-scoped token for an authorizing mirror | Yes |
| `GetCache` | The newest importable build cache of the plan's own scope, chosen by the controller; `NOT_FOUND` when there is none ([build cache](build-cache.md#shared-caches-d26)) | Yes |

## Rules

- **Lease fencing:** every lease-bound request carries `LeaseRef{attempt_id, fencing_generation}`. Stale
  generations are rejected.
- **Assignments** carry only IDs, the plan digest, the isolation profile, a secret-bearing flag, limits, the
  lease deadline and, when tracing is on, a W3C `traceparent`. They never carry secret values, provider references or plan content.
- **Secret release:** `ReleaseSecretResponse` is the only message that carries secret material. Denials have typed
  reasons. "Optional unbound" is the only reason the worker's BuildKit provider maps to "not found"; every other
  denial fails the build (OD-15).
- **One build per daemon:**
  - workers advertise `free_daemon_slots`;
  - the scheduler never over-assigns;
  - a worker rejects an assignment for an occupied daemon with `REJECTION_REASON_DAEMON_OCCUPIED` (OD-16).
- **Reconnection:** `Hello` lists the leases the worker still holds. `HelloAck` returns the ones the controller has
  abandoned, and the worker destroys their environments before accepting work.
- **Cancellation** stops secret release immediately. The worker ends the BuildKit session, destroys the environment
  and reports the cleanup outcome.
- **Compatibility:** majors must match. Minors only add fields and messages. The controller supports the current
  and previous minor, and `HelloAck.negotiated` records the agreed version.
- **Protocol 1.3** (ADR 0045): `Heartbeat` reports the host architecture and free bytes on the update filesystem
  for the checks before a rollout; `UpdateRequest.rollback` asks the host to reinstall its previous release and is
  sent only to workers that negotiated 1.3, because older workers would read it as an update. Older workers keep
  working; their free space shows as unreported.
- **Heartbeat capacity** (any 1.x worker from release 0.1.14): `Heartbeat.capacity` reports disk per filesystem,
  memory and CPUs for the capacity view (`operators/capacity.md`). It is optional; older workers leave it unset and
  show no report, so no protocol minor is needed.
- **Protocol 1.4** (ADR 0048): `Capabilities.providers` lists installed conversion tools by version;
  `ControllerMessage.conversion` assigns a conversion run, and `GetConversionJob`, `GetConversionInput` and
  `ReportConversion` are bound to its lease like build calls. Conversion leases travel in their own `Heartbeat`
  field. Conversions go only to workers that negotiated 1.4; older workers keep building
  (`kubernetes-conversion.md#execution`).
- **Protocol 1.5** (ADR 0049): the plan may carry `dockerfile.baseImagePins`, which the worker builds as BuildKit
  named contexts. A plan with pins goes only to workers that negotiated 1.5, since older workers would ignore them
  and build the moving tag.
  Also in 1.5: `AttemptReport.egress_denied` lists the destinations the worker's egress proxy denied (host,
  port, count, reason; at most 50) and `egress_unrecorded` the decisions beyond its limit, sent with the final
  report. The controller validates and stores them and never derives them from the build log
  (`egress.md#denied-destinations`). Older workers omit them.
- **Protocol 1.7:** `AppendLog` sends build output while the attempt runs, for builds without secrets
  (`build-lifecycle.md#live-logs`). Workers call it only after negotiating 1.7; an older controller never receives
  it, and an older worker simply shows its log when the attempt ends.
- **Protocol 1.6:** the plan may carry `image.labels` and `image.annotations`, which the worker applies to the image
  (`build-execution.md#image-labels-and-annotations`). Every `dockerfile`, `recipe` and `railpack` plan carries at
  least the automatic `org.opencontainers.image.created`, so those builds go only to workers that negotiated 1.6;
  older workers would ignore the field. Until a worker is updated it keeps only `buildpacks` builds and
  conversions.
- **Artifact uploads** use object-scoped grants. Workers never receive registry credentials (ADR 0024).

## Automated checks (`make api-check`)

`tools/apicheck` compiles the contract and enforces:
- a versioned package;
- a `LeaseRef` on every lease-bound request;
- secret-like fields only in `SecretValue.value`, `ReleaseSecretResponse.secret` and
  `EnrollRequest.enrollment_token`;
- nothing reachable from `Assignment` that could carry secrets or plan content.

The checker's tests show each rule rejecting a deliberately broken contract.
