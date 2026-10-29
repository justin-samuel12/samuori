# Capacity and Activity

**Infrastructure > Capacity** (`GET /system/capacity`, organization administrators and operators) shows, in one
place, what each host last reported and what is still in progress. It refreshes every 15 seconds.

## Hosts

| Host | Measured | How often |
|---|---|---|
| Controller | Controller data (`/var/lib/samuori`, the database when it is SQLite, logs), local artifacts (`<dataDir>/blobs`), update staging (`/var/lib/samuori-update`), the root filesystem, memory and CPUs | Every 30 seconds |
| Worker | Builds and worker state (each attempt's workspace), update staging, the root filesystem, memory and CPUs | With every heartbeat (15 seconds) |

Directories on the same filesystem are shown once, with their labels joined. Memory is read from `/proc/meminfo` and
is not shown on other systems. A report older than two minutes is marked **Stale**; a host on a release before this
view, or a worker that has not connected since its update, shows no report. Worker reports are bounded before they
are stored (at most 8 filesystems, labels without control characters).

Disk turns amber below 15% or 5 GiB free and red below 5% or 1 GiB; memory turns amber below 10% available. With
S3 or other object storage for artifacts, uploads and images are not on controller disks; **Infrastructure >
Storage** shows what Samuori keeps there.

## Builds in progress

Unfinished runs (queued, assigned, preparing, building, committing), oldest first: how long each has been in its
state, its worker and attempt. A worker renews its lease every heartbeat; **Lease overdue** means it stopped, and
reconciliation retries the run on a new attempt or fails it once its attempts are used
([`../architecture/build-lifecycle.md`](../architecture/build-lifecycle.md)). Queued runs expire after 24 hours and
every build has a time limit, so no run waits forever.

## Open uploads

Uploads not yet committed, failed or expired, with who started them and how much has arrived. Starting an upload
again instead of resuming creates a new one; the old one counts toward its owner's limit of five open uploads until
it expires, 24 hours after it started, and its staged chunks are then deleted
([`../architecture/sources.md`](../architecture/sources.md)). Identical files are stored once.
