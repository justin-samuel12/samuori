# Messaging, Scheduler and Worker Protocol

Status: **Authoritative** (ADRs 0012, 0017).

## Durable state and messaging

- Authoritative job and attempt transitions are transactional in the database.
- An outbox row is written in the same transaction; an idempotent publisher relays it to JetStream.
- Consumers acknowledge only after durable state updates. Redelivery is expected; unacknowledged messages are
  redelivered after a visibility (ack-wait) timeout.
- Publication is idempotent: retries reuse the same idempotency key and never emit duplicate publish events.
- NATS is a wake-up/transport mechanism, **not the source of truth**. Only controllers connect to it; workers
  receive work over the worker channel (ADR 0024).
- Leases are compare-and-swap with fencing tokens (monotonic generation) and unique idempotency keys. Stale
  results from abandoned attempts are rejected.
- Reconciliation at startup and periodically: missing notifications, expired leases, orphaned workloads,
  incomplete artifact commits, publish retries, worker-version incompatibility.
- Network partitions are normal; a late worker must not overwrite a successor's outcome.

## Implementation (WP17)

- `internal/messaging` runs JetStream inside the controller process with **no network listener**
  (`DontListen`); only in-process clients exist, so workers and other hosts cannot connect (SEC-MSG-01). The relay
  publishes outbox rows with `Nats-Msg-Id`; consumers (`dispatch`, `deliver`, `cancel`) acknowledge only after
  their handler succeeds. Payloads are `{runId, generation}` only (SEC-PLN-03).
- `internal/builds` creates runs after admission: an inadmissible run is stored as `blocked` with its reason; an
  admissible one stores its immutable plan (`build_plans`, digest-addressed) and is `queued`.
- Dispatch takes teams in turn (round robin) and, within a team, priority then age. Each assignment is a
  compare-and-swap on the run that increments the fencing generation and writes the attempt and the outbox message
  together. Eligibility is re-evaluated at every assignment with `isolation.Eligible`.
- Delivery sends an assignment only if the message's generation is still current, at most once per lease.
  `GetPlan`, `ReportAttempt`, heartbeats and rejections all check the lease; stale calls are refused and the old
  attempt is marked `superseded` (CHA-LSE-01).
- Workers verify the plan bytes against the digest bound to the lease before preparing anything (SEC-PLN-02).
- Reconciliation every 5 s abandons attempts whose lease passed its deadline: the run is requeued while
  `attempts_used < max_attempts` (3), then fails as `infrastructure`. Queued runs expire after 24 h. At start-up
  every queued and assigned run gets a fresh wake-up, so losing the message store strands nothing.
- Until artifact storage (WP18), a completed report records the artifact digest without verifying the stored
  artifact; WP18 adds that check before `succeeded`.

## Scheduling

**Invariant (OD-16):** at most one active build per `buildkitd`. The scheduler tracks daemon slots reported by
each worker and never assigns beyond them. Workers enforce the same rule independently, and the worker rejects any
second assignment for an occupied daemon even if the scheduler sends one. Secret-bearing builds always get a fresh
daemon (OD-18). Kubernetes conversion runs hold a daemon slot too and are counted with builds
(`kubernetes-conversion.md#execution`).

**Enrollment is not trust.** A worker enrols **unassigned** (ADR 0045): it is connected and listed, but it is
eligible for no build at all, trusted or untrusted (`unassigned`), the scheduler skips it, it therefore receives no
lease and no secret, and no self-test can start for it. Only an administrator assigns it to a pool
(`PUT /workers/{id}/pool`, audited `worker.assign`). An assigned worker is eligible for untrusted builds only when all
of these hold:
- its pool is enabled: its isolation profile is approved, which creating the pool does in the creating
  administrator's name;
- the worker itself passed a self-test in the last 24 hours, for its current pool, that profile and its current
  configuration digest (ADR 0027, OD-22); `worker-self-test-stale`, `worker-self-test-failed` or
  `worker-config-changed` otherwise. Self-tests are counted per pool, so reassigning a worker resets its trust even
  between pools with the same profile, and a test still running for the old pool is closed as failed. A worker with
  builds in progress or a pending removal cannot be reassigned;
- it is not cordoned, quarantined or revoked;
- its version is compatible (`version-incompatible`);
- it builds every requested platform (`platform-unsupported`; admission tells the developer to choose a supported
  processor architecture).

Eligibility is re-evaluated at every assignment.

**Locations (D25).** Locations are managed records (`/locations`); every pool belongs to one, and creating a pool
registers its location. Every organization starts with one location, `default` (ADR 0045); a pool created without a
location goes to the organization's only location, or `default`. The UI hides location fields while only one
location exists. An administrator may restrict a project to locations (`PUT /projects/{id}/locations`, data
residency); admission and the scheduler then consider only pools in those locations. A build request may name a
preferred location (`location` in `POST /builds`), which must be allowed for the project; the scheduler tries it
first and otherwise uses any allowed location. Pools can be renamed or moved (`PATCH /worker-pools/{id}`); changing an
enabled pool's isolation profile or trust domain keeps it approved by the editing administrator, recorded as an
approval decision and audited, and its workers need a new self-test for a new profile. `PUT
/worker-pools/{id}/approval` disables or re-enables a pool. A pool that ever had workers cannot be deleted, so
worker history is kept; `DELETE /worker-pools/{id}` then answers 409 with the number of workers, and the UI offers to
disable the pool instead. Administrators add and delete locations under Nodes & workers → Locations
(`POST /locations`, `DELETE /locations/{name}`). Deleting a location is refused (409) while a pool, a project's
location restriction or a federation route uses it, and the refusal names each use; a route's location is deleted
with the route (`docs/operators/federation.md`). Locations have no rename: add a new location and move pools and
projects to it. The run records the isolation profile of the pool that took it, and
the assignment carries that profile; the plan's `isolationProfile` is fixed when the plan is resolved, before a pool
is chosen. An attempt's failure class and detail (masked by the worker) are shown on the build page.

Match capabilities, OS/architecture/engine, isolation profile, quotas, capacity, priorities, location/data
residency and policy. Users may express authorized preferences (e.g. location) only. Queue order prevents
starvation while respecting admin priority, per-team fairness and concurrency limits. Incompatible
engine/OS/architecture requests are rejected at admission (before `queued`) with an actionable message.

## Worker enrollment

1. An authorized admin creates a short-lived, single-use enrollment token. It names no pool: the worker enrols
   unassigned and is assigned afterwards (ADR 0045). Only the all-in-one local worker's token, issued by the
   controller itself, names the `local` pool. Raw tokens are shown once; never emailed, logged or reusable.
   `GET /enrollment-tokens` lists recent tokens without values (issued by, used by which worker, revoked, expired).
   An administrator may revoke an unused token before it expires (`DELETE /enrollment-tokens/{id}`, audited as
   `enrollment_token.revoke`); enrollment checks revocation in the transaction that consumes the token, so a revoked
   token never enrolls a worker. A used token cannot be revoked (409); revoke the worker it enrolled instead.
2. The worker pins the controller's trust material (fingerprint supplied out of band with the token).
3. The worker initiates an outbound TLS connection and proves possession of its private key via CSR.
4. The controller issues a unique, rotating, revocable mTLS identity (internal CA by default; OpenBao PKI optional,
   Vault PKI compatible).
5. Operators can cordon, drain, quarantine or revoke workers; certificate rotation is automatic before expiry.

## Worker channel

- Cache rules for secret-bearing builds: `build-cache.md`.

- gRPC over mutual TLS, initiated outbound by the worker, with server streaming for assignments (OD-20). No inbound
  ports on workers. The contract lives in `api/proto/` (M0 task).
- Scoped authorization: a worker fetches only its assigned source snapshot and build plan (verified against the digest on its lease), and uploads only
  designated results to pre-authorized locations.
- Worker identity never authorizes admin or user APIs.
- In all-in-one, the local worker is a separate process under a separate OS user (ADR 0012); untrusted image
  assembly never runs inside the API process.

## HA and federation

- Controller HA requires PostgreSQL, shared durable messaging, and explicit leader/ownership coordination
  (DB-based leases for singleton duties like reconciliation). Implemented by ADR 0034: membership and a fenced leader
  lease, ownership of in-process work, shared CA material, worker routing across controllers, and failover of
  running builds ([operator guide](../operators/high-availability.md)).
- Controller removal (ADR 0045) is keyed by host name: a `controller_removals` row makes that host's controller give
  up the lease and stop contending, so a peer takes over with a higher epoch before the host wipes. The leader then
  deletes the host's membership rows under its fenced epoch and records its API certificate pins; `JoinAs` refuses
  those pins and admits a new identity issued by an accepted join. Each remaining controller asks its root helper to
  drop the host from its NATS routes and the JetStream meta group with a per-host system account. Removal is refused
  for the last controller, a lost NATS majority, a peer that is down, or a running rollout
  ([operator guide](../operators/high-availability.md#remove-a-controller)).
- Federation is a separate protocol: mutually authenticated controller trusts, declared allowed scope, no ambiguous
  authority over the same build, idempotent forwarding, defined partition behavior. NATS leaf nodes alone are not a
  federation authorization model. Implemented by ADR 0037: peers with pinned CAs, grants and routes, delegations
  that forward specs and source (never plans), validation at the originator, and partitions that wait rather than
  reassign ([operator guide](../operators/federation.md)).
