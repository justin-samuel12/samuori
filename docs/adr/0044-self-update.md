# 0044. Self-update from the portal

Status: Accepted (2026-10-22) — owner decision. Implemented and exercised on two VMs (docs/operators/upgrades.md#self-update). Partly superseded by ADR 0045
(2026-10-22): one signed bundle replaces the three-file upload, controllers update with the leader last, workers in
waves per pool, a failed host pauses the rollout (Resume retries it), Pause/Resume replace Continue, and a worker
whose self-test fails is rolled back to its previous release (worker protocol 1.3). Signing, host-side verification
and root-applied installs below are unchanged.

## Context
Upgrading meant copying a release to every controller and worker and rerunning the installer on each, in order
(`docs/operators/upgrades.md`). The owner wants one button that updates the controllers and then lets the update
flow down to the workers. Applying a release replaces binaries as root, so a portal account alone must never be able
to run code on the hosts.

## Decision
- **Signed releases only.** A release is the `make release-artifacts` output: the archive, `SHA256SUMS` and its SSH
  signature (namespace `samuori-release`, principal `release@samuori.invalid`). Each host trusts only the keys in
  `/etc/samuori/release-signers` (root-owned, installed with `install.sh --release-signers`). Without that file the host
  refuses every self-update.
- **Root applies, the service only stages.** The controller or worker writes the archive, checksums, signature and a
  request naming the version into `/var/lib/samuori-update` (group `samuori-update`, 2770). `samuori-self-update.path` starts
  `samuori-self-update` as root, which copies the files out of the staging user's reach, verifies the signature and the
  checksum of `samuori-<version>-linux-<arch>.tar.gz`, and runs that release's `install.sh` with the options recorded in
  `/etc/samuori/install-args` at installation. Options of `init` and `join`, which carry secrets, are never recorded.
- **Rollback.** The installer keeps a copy of the installed release in `/var/lib/samuori-release/current`. The helper
  copies it to `previous` before installing; if the install or the service afterwards fails, it reinstalls
  `previous`. It reports `succeeded` or `failed` with the reason in `/var/lib/samuori-update/result`.
- **Rollout order** (portal): administrators upload a signed release, which controllers verify and keep in blob
  storage. The leader updates controllers one at a time, then drains and updates workers one at a time; each step
  waits for that host to report the new version. A worker then runs its own self-test, because its configuration
  changed, and counts as updated only when that passes. A worker whose step fails is reactivated: it either never
  installed the release or went back to its previous one. Progress lives in the database, so a leader that
  restarts for its own update hands over to the next leader.
- A rollout may pause after its first worker (on by default in the portal), so an administrator checks one updated
  worker before the others; Continue is audited. Controllers always go first, because a worker never runs a newer
  release than the controllers (ADR 0036).
- Workers fetch the release over their mTLS session (`GetRelease`, worker protocol 1.1) only while the rollout's
  current step targets them, and report a failed install with `ReportUpdate`.

## Consequences
- Hosts installed before this change must be reinstalled once with `install.sh` (and `--release-signers`).
- The release key becomes the trust anchor for every host; keep it offline and never on a build worker.
- A release that changes the installer's options or prerequisites still installs, because it runs its own installer.
