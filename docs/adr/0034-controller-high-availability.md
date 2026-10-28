# 0034. Controller high availability

Status: Accepted (2026-10-20). Implements the approved HA direction in `docs/architecture/scheduler-and-workers.md`
and ADR 0017 (D07, D08).

## Context
M11 requires several controllers sharing one PostgreSQL database and a clustered NATS, with explicit leadership and
ownership, while standalone and all-in-one installations stay unchanged and need no external infrastructure.

## Decision
- **Topology.** `cluster.enabled` runs a controller (`role: controller`) as one of several. It requires a
  PostgreSQL `database.dsn`, an external NATS cluster (`messaging.url`, TLS and credentials off loopback, stream
  replicas `messaging.replicas`, default 3), blob storage shared by every data class (S3 or OCI routes) and one
  cluster key (`cluster.keyFile`). Configuration that misses any of them is refused at start-up. The API needs no
  sticky sessions; workers list every controller's worker endpoint.
- **Same code for one controller.** Every controller, standalone included, registers in `controllers`, heartbeats
  every 3 s and contends for the `leader` lease (10 s) in `cluster_leases`. Taking an expired lease increments its
  epoch; `Fenced` checks the epoch inside a transaction. A controller is dead 15 s after its last heartbeat, or at
  once after a clean stop. Each start is a new member, so work claimed before a crash is recognisably orphaned.
- **Clock.** Controllers compare their clock with PostgreSQL's at start-up and on every heartbeat (health check
  `clock`, at most 2 s apart); a controller with a skewed clock gives up leadership.
- **Singleton duties** run only on the leader: artifact garbage collection, CA retirement, audit export and purge,
  notification routing, controller pruning, and re-waking queued builds when leadership changes. Each is idempotent,
  so one overlapping iteration during a handover does no harm.
- **Ownership of in-process work.** Uploads being committed, discoveries, validation runs, destination locks and
  pipeline fetches record the controller that runs them. Reconcilers, now periodic, reclaim only work whose
  controller is dead; work of live controllers is never failed or restarted. Validation verdicts and notification
  results are recorded only under the claim token they were taken with, so a superseded controller's late result is
  discarded. Completion writes of uploads and discoveries are compare-and-swap on state and owner.
- **Shared material.** The cluster key is the secret store key: operators place the same key file on every
  controller (`samuori-server new-cluster-key`, or a standalone controller's `keys/secrets.key` when it joins). The
  internal CA directory (CAs, trust bundle state, grant key) is mirrored through `cluster_material`, encrypted with
  a key derived from the cluster key and written with a revision check; other controllers reload it within 10 s and
  pass a changed bundle to their workers. Policy dry-run confirmations and controller signals use keys derived from
  the cluster key.
- **Worker routing.** A worker holds one stream to one controller; `worker_sessions` records which. A message for a
  worker connected elsewhere is forwarded over core NATS to that controller in an envelope signed with a derived key
  and bounded by a one-minute timestamp window. A connecting worker closes its older stream on other controllers,
  and is sent again every assignment leased to it that it has not started. Free daemon slots are counted again under
  a row lock on the worker during assignment (OD-16). The worker agent takes several endpoints and moves to the next
  when a session fails; its calls follow the current session, and attempt reports are retried across reconnects
  for up to five minutes. Shutdown ends worker streams with `Unavailable`; only revocation sends `PermissionDenied`.
- **Sign-in lockout** is kept in the database, so a cluster grants one failure budget, not one per controller.
- **Bootstrap.** With setup incomplete, a starting controller issues a bootstrap token only if no unexpired one
  exists; the controller holding it logs its path.

## Consequences
- Per-address rate limits stay per controller; a client spreading requests over N controllers gets N budgets.
- Notification delivery stays at least once: a controller that stops after sending and before recording causes one
  resend.
- Every controller relays the shared outbox; JetStream's message-ID deduplication and idempotent consumers absorb
  the duplicates.
- The audit export directory must be shared storage when exports are required for purges.
- A lost controller signal only delays work: reconnection resends assignments, and leases bound everything else.
- Federation (D25) is a separate protocol and is not part of this ADR.
