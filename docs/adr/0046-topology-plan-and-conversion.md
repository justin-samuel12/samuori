# 0046. Topology plan, resilience tier and conversion

Status: Accepted (2026-10-23) — owner decisions. Implementation in progress (roadmap).

## Context
`samuori-setup` asks only "cluster or single-server". Nothing tells an administrator how resilient the result is, what a
given number of controllers and workers buys, or what is missing for high availability, and a single server cannot
become a cluster (or back) without reinstalling. Controllers coordinate through NATS JetStream, which needs a
majority of members, so two controllers tolerate no more failures than one.

## Decision
- **Setup asks for a plan.** `samuori-setup` asks for the deployment (single server or cluster) and, for a cluster, the
  number of controllers and workers. It then shows the tier the plan reaches, what each component can lose, and
  what is still missing for HA. An even controller count is allowed after a warning that it tolerates no more
  failures than one fewer.
- **The plan is a target; the tier is measured.** The plan is stored (migration 00052) and shown on the Servers
  page as progress ("2 of 3 controllers joined"). The tier shown is always computed from live members and the
  measured or declared properties below, never from the plan alone. Administrators change the target there.
- **Samuori works out the tier.** Each component reports how many failures it tolerates:
  - controllers: a NATS majority, so ⌊(n−1)/2⌋ of n running controllers;
  - workers: n−1 of the n online, self-tested workers in each enabled pool;
  - database: none for SQLite; for PostgreSQL, one when the administrator declares it replicated (for example RDS
    Multi-AZ), which Samuori cannot measure and labels "declared";
  - blob storage: none for local disk; one for S3; one for an OCI repository only when declared replicated;
  - entry point: none when the portal URL names a controller directly, one when it names something else (a load
    balancer).

  **Standalone** means every component tolerates no failure. **HA** means every component tolerates at least one.
  Anything between is **Partial HA**, and the Servers page lists exactly which components still have no
  tolerance and what to add. A plan's tier is computed the same way from the planned counts.
- **Scaling within a cluster** uses the existing join and removal flows (ADR 0045).
- **Conversion in both directions.** `samuori-server topology convert` runs on the host, during a maintenance window,
  with Samuori stopped:
  - single server → cluster copies every table from SQLite to an empty PostgreSQL database (same schema version,
    row counts compared), copies every stored blob from local disk to S3 or an OCI repository (digests compared),
    then rewrites the host as the first controller with its worker kept. Other controllers then join normally.
  - cluster → single server is allowed only when one controller remains (remove the others first). It copies
    PostgreSQL to SQLite and blobs to local disk, compares them, and rewrites the host as a single server. Separate
    workers stay enrolled; the host is required to be a dedicated VM again (ADR 0027).
  - The source is never modified. A failed check leaves the source in place and the host on its old configuration.
    Both conversions are audited.
  - Amended 2026-10-23 (OD-28): the converted controller keeps its local worker, and it updates together with its
    controller. `install.sh --role controller --keep-local-worker` installs and upgrades the worker parts too and
    is recorded for later upgrades. A rollout updates that worker in its controller's step: drained, then the host
    updates, then the worker's self-test, and a failed self-test rolls the host back. Removing the controller drains
    and revokes the worker.

## Consequences
- Administrators see an honest tier and a concrete list of gaps instead of a label.
- Declared properties (database replication, OCI replication) are shown as declared, not verified.
- Conversion is a maintenance-window operation; it is not live migration.
- A converted single server keeps its data, users, history and images.
