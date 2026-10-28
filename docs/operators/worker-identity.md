# Worker identity runbook

Status: **Current for WP24.** Design: ADR 0026. Protocol: [`../architecture/worker-protocol.md`](../architecture/worker-protocol.md).

Workers prove who they are with a client certificate from the controller's internal CA and pin that CA, not a
server certificate. Enrollment establishes identity only: a worker joins unassigned and builds nothing until an
administrator assigns it to a pool, and it builds uploaded source only after it passes its own self-test for that pool
(ADR 0045).

## Add a remote worker

On the controller, accept workers from other hosts by listening beyond loopback and naming the controller as workers
reach it:

```sh
sudo ./install.sh --release . --confirm-dedicated-vm --worker-listen 0.0.0.0:8444 \
  --worker-hostnames localhost,127.0.0.1,samuori.example.com
```

Add `--no-local-worker` when this host should run no builds itself, as a pure controller for remote workers. The
controller then creates no local pool and its own worker stays switched off. Pass it on every upgrade too: without
it, re-running the installer switches the local worker back on. A worker host refuses the option.

Firewall the worker port to your build network. Every call needs a certificate from the CA, and enrollment needs a
single-use token, but nothing else is served there.

In the UI, **Administration → Servers**, choose **Add a worker** (or **Enroll a worker** under Nodes & workers). On
each remote build VM (a dedicated VM, like the all-in-one host), from the release folder:

```sh
sudo ./samuori-join worker samuoriwj_...
```

The code holds the controller worker endpoints, the CA pin and the token; `samuori-join` checks the pinned certificate,
installs the worker and enrols it (`high-availability.md#join-a-worker`). The two-step form still works:
`sudo ./install.sh --release . --confirm-dedicated-vm --worker-only --controller samuori.example.com:8444`, then
`sudo samuori-enroll-worker <CA pin>` with the token pasted. The token is single use, expires within an hour, is
stored only as a hash and names no pool. The worker is named after its host and appears as **Unassigned**: connected,
but it gets no builds, no secrets and no self-test. Choose **Assign to pool** on its row (administrators only) and pick
a pool; the worker's self-test then runs for that pool's isolation profile.

**Move to pool** reassigns a worker. Trust never moves with it: only self-tests recorded for the worker's current pool
count, so it must pass a new one, even when both pools use the same profile. A self-test still running for the old
pool is closed as failed. A worker with builds in progress, or being removed, cannot move; drain it first. Every
assignment is audited as `worker.assign`.

Creating a pool approves its isolation profile in the creating administrator's name; there is no separate approval
step. Changing an enabled pool's profile or trust domain keeps it approved by the administrator who saved the change,
recorded as an approval decision and audited. **Disable pool** withdraws approval (its workers then build no uploaded
source) and **Enable pool** restores it, each with a recorded reason. The all-in-one `local` pool is created by the
controller, not an administrator, so it starts disabled: choose **Enable pool** on it.

Organizations start with one location, `default`, and new pools go there. Location fields stay hidden until a second
location exists; **Put this pool in another location** in the pool dialog adds one. An organization that already had
locations before this release keeps them, and new pools default to its only location.

## Renewal

Worker certificates last 7 days. Each worker renews at two-thirds of that over its existing connection, with a new
key pair. The controller accepts the previous certificate only until the new one is first used, so a stolen old
certificate stops working as soon as the worker reconnects. A renewal interrupted by a crash is completed or rolled
back when the worker starts.

## Expired workers

A worker offline for longer than its certificate lifetime stops with "the worker certificate has expired" and exit
status 3; systemd does not restart it. Enroll it again with a new token (`sudo samuori-enroll-worker`). Re-enrolling
with the same worker name revokes the old identity automatically; the new identity starts unassigned, so assign it
to its pool again. The all-in-one `local` worker is re-enrolled by the
controller on its own.

## Clock skew

Workers warn when their clock differs from the controller's by more than two minutes, and certificate errors caused
by time are reported as "clock skew". Fix time synchronization (NTP) on the worker or the controller.

## Cordon, drain, quarantine

**Manage** on a worker (operators and administrators):

| Action | Effect |
|---|---|
| Cordon / uncordon | Stops or resumes new builds; running builds continue |
| Drain | Stops new builds and asks the worker to finish what it runs |
| Quarantine | Stops scheduling while you investigate; the identity stays valid |

## Remove

**Remove** (administrators) drains the worker, then sends it a remove request over its connection. The worker writes
a wipe request that `samuori-wipe.service` (root) carries out with `samuori-uninstall`: it stops and deletes every Samuori service,
`/etc/samuori`, `/var/lib/samuori*`, the downloaded tools, AppArmor profiles, the sudoers entry, the Samuori users and groups, and
the folder the installer ran from (recorded in `/etc/samuori/install-source`). The worker is then revoked. Operating-system
packages and system logs stay; deleted files can remain on the disk until overwritten, so terminate the instance for
full assurance. A worker that is not connected, or predates protocol 1.2, is revoked and shown as **not confirmed
wiped**: run `sudo samuori-uninstall` on the host if it comes back. `sudo samuori-uninstall` also works by hand on any host.
Exercised 2026-10-22 on two Lima VMs (worker removed and wiped in five seconds; manual uninstall on the controller).

## Revoke

Revocation (administrators) is immediate and permanent: every request with the identity is denied, its open
connection is closed, its leases are fenced, and its running builds are requeued on other workers. A late report from
the revoked worker is rejected. The worker stops with exit status 3.

### Suspected compromise

Revoke with **The worker may be compromised** and a time window. Every image the worker built in attempts started
in that window is quarantined: publication refuses it with the `worker-trust` gate. Review each image under
**Administration → Quarantined images** (security reviewers or administrators) and release it with the review
result, for example after rebuilding it on a clean worker and comparing digests. Releases are audited.

## Planned CA rotation

1. **Administration → Certificates → Rotate the CA**. A new CA joins the trust bundle; connected workers receive it
   within seconds and acknowledge it with their next heartbeat.
2. Wait until **Waiting for workers** is empty. Offline workers must connect once; a worker you no longer need can be
   revoked instead.
3. **Switch to the new CA**. New certificates, including the controller's serving certificate, come from the new CA.
   The old CA stays trusted until every certificate it issued has expired (7 days plus the serving certificate
   lifetime), then it is removed from the bundle and its key is deleted.

New enrollment tokens carry the new CA pin as soon as issuance switches.

## Emergency CA rotation

Use when the CA key may be compromised: **Administration → Certificates → Emergency rotation**. Every CA is replaced
at once, its key deleted, and every worker identity revoked; running builds are requeued. Nothing signed by the old
CA is trusted afterwards, so each worker must be enrolled again out of band with a new token, which carries the new
CA pin. The all-in-one `local` worker is not re-enrolled automatically after an emergency rotation: re-enroll it like
a remote worker, after confirming the host is clean.

## CA key custody

The CA keys live in `<dataDir>/pki` (mode 0600) with `authority.json`. Include them in encrypted backups (D27). If
the keys are lost without a backup, the controller cannot start: stop it, move `authority.json` and the `ca*.crt`
and `ca*.key` files aside (keep the artifact grant key in the same directory), start it to create a new CA, revoke
every worker and enroll each again.

## Evidence

> History: the tests named below ran at earlier commits and were removed (ADR 0039); restored focused tests are
> marked in `docs/developers/security-test-catalog.md`.

Unit and protocol tests: `internal/pki/authority_test.go`, `internal/workeragent/identity_test.go`,
`internal/builds/identity_test.go`, `internal/publication/publication_test.go` (SEC-WRK-04, 05, 07, SEC-ID-01..06).

Two VMs (2026-10-19, macOS host, Lima on Apple Virtualization with the `user-v2` network, Ubuntu 26.04 arm64):
`samuori-clean` installed as a controller with `--worker-listen 0.0.0.0:8444`, and `samuori-remote`
(`deploy/dev/lima/samuori-remote.yml`) installed with `--worker-only --controller lima-samuori-clean.internal:8444`. The remote
worker enrolled, received the trust bundle, passed its own self-test and became eligible. Re-enrolling it with a new
token revoked its previous identity. A planned CA rotation refused to switch until both workers had acknowledged the
new bundle, then switched; the remote worker reconnected against the new serving certificate. Revoking the remote
worker through the API closed its stream at once and every reconnect was refused. A first attempt on Lima's `vzNAT`
network failed because the VMs could not reach each other; `user-v2` was used instead. Remote workers on other
hypervisors, cloud networks and amd64 are not covered yet.

Two-host acceptance (2026-10-21, `make test-acceptance-two-node`): a controller VM installed with
`--no-local-worker` and a worker VM installed with `--worker-only`. In the browser, the administrator created a pool, enrolled the
worker with a single-use token and approved the pool; the worker passed its self-test in under a minute; a
developer's build ran on the remote worker, passed validation and was published to the controller's registry. The
test checks every attempt ran on the remote worker. 5/5 passed.
