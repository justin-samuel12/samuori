# Upgrades

## Version matrix (ADR 0036)

| Component | Supported against controller release X.Y |
|---|---|
| Other controllers on the same database | X.(Y−1) to X.(Y+1), during a rolling upgrade only |
| Workers | X.Y and X.(Y−1); never newer than the controller |
| Database schema | Migrated by the newest controller; an older controller refuses a newer schema |
| PostgreSQL | 16–18 |

The Upgrades & Diagnostics screen shows where every controller and worker stands.

## Upgrade a standalone or all-in-one installation

1. Configure backups if you have not ([backup and restore](backup-restore.md)). The controller then writes a backup
   before it migrates.
2. Install the new release and restart. The log reports the backup and the migration, and the Upgrades &
   Diagnostics screen lists the upgrade.

## Rolling upgrade of a cluster

1. Upgrade controllers one at a time. The first one migrates the schema (one at a time across the cluster) and writes
   the pre-upgrade backup; the others keep serving.
2. Drain each worker (**Nodes & workers > Drain**), wait until it has no build, upgrade it and let it reconnect; it
   re-runs its self-test if its configuration changed. Workers on the previous minor release keep working until then.
3. Check the Upgrades & Diagnostics screen: every controller compatible, every worker accepted.

## Roll back

A controller never runs on a newer schema. To go back, stop the controllers, restore the pre-upgrade backup
(`samuori-server restore`) into an empty data directory and database, and start the previous release on it. Builds and
changes made after the upgrade are lost.

## Self-update

Status: **implemented and exercised** for the three-file upload (2026-10-22, two VMs, arm64): the host side refused
a tampered archive and a foreign signature and rolled a failing installer back; from the portal a controller and a
remote worker updated in order. The single bundle, the checks before updating, waves and remote rollback (ADR 0045)
have focused tests only; not yet exercised on VMs, with several controllers, amd64 or AWS.

Each host applies only releases signed by a key in `/etc/samuori/release-signers`:

1. Create the release key once with `make release-key`: `dist/signing/release` stays on the build machine (back it
   up), and `dist/signing/allowed_signers` goes to the hosts.
2. Install every host with `--release-signers allowed_signers` (hosts installed earlier: rerun `install.sh` once).
   The installer records its options in `/etc/samuori/install-args` (never the `init` or `join` options) and keeps the
   release in `/var/lib/samuori-release/current`.
3. Build one signed bundle with `make update` (`UPDATE_ARCHES="amd64 arm64"` by default; the version comes from the
   `VERSION` file and the commit). It first empties `dist/`, keeping `dist/signing` and any directory holding
   `RELEASE_KEY` or `RELEASE_SIGNERS`, then writes
   `dist/samuori-<version>.samuoriupdate`: a tar with `manifest.json` (version, worker protocol, architectures), one release
   archive per architecture, `SHA256SUMS` over all of them and its signature `SHA256SUMS.sig`.
4. In **Upgrades & Diagnostics > Self-update**, upload the bundle. The controller verifies its structure, every
   checksum and the signature before it keeps anything. **Changes** lists the bundle's commit history (one line per
   change, from `git log` when the bundle was made, kept as `CHANGES.tsv` inside each release archive so the
   signature covers it and controllers before 0.1.4 accept the bundle): what is new since the version this
   controller runs, and the rest folded under **Already running**. Bundles made before the history was added show
   none.
5. **Check and update** shows the same changes, runs the checks and shows each result and the planned waves:

   | Check | Fails when |
   |---|---|
   | Signature | the bundle is not signed by a key in this controller's `release-signers` (checked again now) |
   | Version path | the release is older than a host's (unless **Allow a downgrade**, which is audited), every host already runs it, or a host is more than one minor version away |
   | Worker protocol | the bundle's worker protocol major differs, or its minor is older (unless downgrade is allowed) |
   | Every host reachable | a controller has no live heartbeat or a worker to update is not connected |
   | Free disk space | a host has less than four times its archive free on its update filesystem (`/var/lib/samuori-update`); hosts on releases before the bundle do not report it, which is a warning |
   | Architectures | a host runs an architecture the bundle lacks, or reported none |

   Warnings (a host that reports no free space, an allowed downgrade, workers left out) do not block. **Update**
   runs the checks again on the server and refuses to start if one fails. No reason is asked; the audit log records
   who started which version (`POST /system/rollouts` still accepts an optional `reason`).

The leader updates controllers one at a time, itself last, so leadership moves once. Then each pool's workers update
in waves of the chosen share of the pool (25% by default, at least one), all pools in parallel, each wave spread
across locations. Every worker is drained, waits for its builds, fetches the release over its mTLS session, comes
back on the new version and runs its self-test; the next wave of that pool starts only when the whole wave passed.
With **Pause after the first wave of workers**, the rollout pauses once every pool's first wave passed; check it
(for example with a build), then **Resume**.

**Everything at once** (wave size 100%, ADR 0045 as amended 2026-10-25) is for short maintenance windows:

1. One secondary controller, picked at random among the reachable ones that need the release, updates alone. The pick
   follows from the version, so the checks and the rollout name the same host. With a single controller, it goes
   alone first.
2. Once it is back on the new release, the leader hands it the leader lease directly (the old leader is fenced at
   once; if the new one died meanwhile, the lease expires and the controllers elect a leader as usual).
3. The new leader then updates every other controller, including the old leader, and every worker together. Every
   worker drains at the same time, so builds wait until the workers are back.

With **Pause after the first controller**, the rollout pauses after step 1. A failed host still goes back to its
previous release and pauses the rollout. Below 100%, the order above applies. Verified on three controller VMs (`deploy/dev/lima/cluster.sh update-all`, 2026-10-25); a run with workers is outstanding.

A controller converted from a single server keeps its worker (`install.sh --role controller --keep-local-worker`,
ADR 0046, OD-28). That worker is never in a wave. It is part of its controller's step, shown as
"<host> with worker local": the worker is drained first, then the host installs the release, which updates both
the controller and the worker. Once a controller process started after the install runs the release, and the worker
is back on it, the worker runs its self-test. The step succeeds only when that self-test passes. A failed self-test
rolls the whole host back, controller and worker together, and pauses the rollout. If the worker is not active, is
unassigned, or its pool cannot run self-tests, the host is still updated, but the worker is named as left out and
gets no self-test.

A failed host pauses the rollout; hosts already updating finish, no new one starts:

- an install or service start that fails: `samuori-self-update` reinstalls the previous release itself;
- a worker whose self-test fails on the new release: the controller drains it again and asks its host to reinstall
  the previous release (worker protocol 1.3; older workers stay on the new release, build no uploaded source, and
  are rolled back by hand, see below).

**Resume** retries the failed hosts; **Stop** ends the rollout, reactivates draining workers and lets hosts already
installing finish. **Pause** stops new hosts at any time. The page polls while a rollout runs and keeps polling
while a controller restarts for its own update. The history lists past rollouts with each host's steps. Signed-in users stay signed in through an update: sessions live in the database. Authenticated API responses carry
the controller's release in `X-Samuori-Version`, and every open portal tab compares it with the release it loaded; within
one session check (two minutes) of a newer release it shows *Samuori was updated to …* with **Reload**. Nothing reloads
on its own, so a form being filled in is not lost. **Clear history** (`DELETE /system/rollouts`) deletes every finished rollout and its steps; a running rollout stays, and the audit log keeps each `rollout.start` and records the purge (`rollout.purge`). The schema upgrade list comes from the audit log and is not cleared.

Workers whose pool profile is not approved, or is `vm-per-job` (which cannot run self-tests yet), unassigned workers
and workers that are not active are left out and named; fix them and update again. On each host,
`samuori-self-update` (root) verifies the release itself, runs its installer with the recorded options, and reinstalls
the previous release if that fails; the outcome is in `/var/lib/samuori-update/result`. Workers need protocol 1.1, so
workers installed before self-update are reinstalled once by hand.

### Rollback

To roll a host back by hand to the release kept before its last update, run
`echo rollback | sudo tee /var/lib/samuori-update/request`; `samuori-self-update` reinstalls
`/var/lib/samuori-release/previous` and records the outcome in `/var/lib/samuori-update/result`. Releases uploaded as three
files before the bundle are listed but cannot be rolled out; upload the bundle instead.
