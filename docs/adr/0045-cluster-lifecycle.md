# 0045. Cluster lifecycle: first node, joining, updates and removal

Status: Accepted (2026-10-22) — owner decisions. Implementation in progress (roadmap). Built: worker removal with remote
wipe, `samuori-uninstall`, and the two-phase controller join; `samuori-join controller <code>` and `samuori-join worker <code>`
in the release folder, with one worker join code (`samuoriwj_...`) that is checked against the pinned CA before
anything installs (SEC-WRK-11, TM-115; focused tests only, not yet run on a VM), and the **Servers** page with both
lists, join requests, join codes and removal (see Implementation notes); workers joining unassigned
and being assigned or moved to a pool (SEC-WRK-10); creating a pool as its approval, with **Disable pool** and **Enable
pool** in place of the approval click; one `default` location with location fields hidden until a second exists; session
expiry (`../architecture/identity.md#sessions`); the command-line first-node setup `samuori-setup` with an answers file
(`../operators/all-in-one.md#install-with-samuori-setup`; not yet run on a VM, RDS or EC2). Controller removal (refusals,
leadership handover, wipe, revocation and NATS member removal through a per-host system account;
`../operators/high-availability.md#remove-a-controller`, SEC-HA-05..09; focused tests and an in-process NATS spike only,
not yet on VMs). NATS route sync with route verification (amended 2026-10-23, OD-27; SEC-HA-10, TM-114; focused tests
and the in-process spike only, not yet on VMs). Updates from one bundle (`make update`; checks before updating; controllers one at a time with the
leader last; worker waves per pool in parallel; pause, resume, stop and history; remote rollback of a worker whose
self-test fails; migration 00048, worker protocol 1.3, SEC-UPD-01..04, TM-113;
`../operators/upgrades.md#self-update`; focused tests only, not yet on VMs). The other parts are not built yet.
Amended 2026-10-25 (owner): at 100% a rollout updates one randomly chosen secondary controller first, hands it the
leader lease once it runs the new release, and then updates every other controller and every worker in one wave;
below 100% the order above is unchanged (`../operators/upgrades.md#self-update`).

## Context
The first AWS installation showed the lifecycle to be too clunky: a long `init` command with a dozen flags, a
separate preparation script, three files per update, different join and enroll procedures, a pool approval click on
top of the self-test, workers bound to a pool at enrollment, and no way to remove a server. The owner set the decisions
below. Samuori stays one organization (D30); locations stay optional
(D25).

## Decision
- **First node: command-line setup.** `sudo ./samuori-setup` asks for the deployment (cluster or single server), this
  host's DNS name, the portal URL, the database, S3, the organization and administrator, and the release public key.
  It tests each answer before moving on (sign-in to the database, write/read/delete on S3, name resolution), takes
  defaults from the host (DNS name, AWS region, RDS CA bundle), never reads secrets from the command line, and
  installs only after a summary. The same answers can come from a file for automation.
- **Tokens only open a join request.** Controller and worker tokens keep today's mechanism: issued in the portal,
  single use, expiring within an hour, stored hashed, revocable, shown once. A token grants no access and carries no
  secrets. Controller and worker tokens are distinct and cannot be used for the other role.
- **Joining a controller is two-phase.** `sudo samuori join controller <token>` checks the pinned certificate, sends a
  request with a verification code and waits. An administrator accepts or rejects it under **Servers**; only on
  acceptance does the controller send the configuration and keys, to that host only.
- **Joining a worker, then assigning it.** `sudo samuori join worker <token>` enrols the worker unassigned: connected, but
  with no builds, secrets or self-test. Someone assigns it to a pool (and may reassign it later); its self-test then
  runs against the pool's profile and decides whether it builds uploaded source. Creating a pool is its approval.
- **Updates from one bundle.** `make update` writes one signed bundle with every architecture. Uploading it runs
  checks first (signature, version path, reachability, disk, architectures). **Update** then moves controllers one at
  a time and workers in waves per pool (default 25%, at least one, pools in parallel, spread across locations), each
  worker drained, updated and self-tested, with live progress, automatic rollback per host, pause, resume, stop and
  history.
- **Removal wipes the host.** **Remove** drains the server, has a root helper on that host delete everything Samuori
  installed (services, units, `/etc/samuori`, `/var/lib/samuori*`, kept releases, downloaded tools, AppArmor profiles,
  sudoers entry, system users, and the folder the installer ran from), then revokes it and drops it from the cluster.
  Removing the last controller, or one that would leave messaging without a majority, is refused. An unreachable
  host is removed and revoked but reported as not confirmed wiped, with the uninstall command and terminate advice; only
  then is changing the database password recommended. Operating-system packages and system logs stay. Coming back
  means installing again. The manual command is `sudo samuori-uninstall`.
- **NATS routes list exactly the members (amended 2026-10-23, owner decision on OD-27, option 1).** Every member's
  routes hold the current members, rewritten on join and on removal by a root helper on each host
  (`samuori-server cluster routes`, `samuori-nats-routes.path`), and route TLS uses `verify_cert_and_check_known_urls`, so a
  removed host that was not wiped is refused even with a certificate the cluster CA signed. The rewrite is atomic per
  member (a reload that does not take restores the previous file), idempotent and retried; it never drops a route to a
  connected peer that was not removed, and never leaves a member without a peer. Verification turns on only once every peer the member is connected to is
  listed, which is also how existing installs converge. A route to a removed host that survives the reload (NATS
  gossip) is dropped by restarting that member, one member at a time under a cluster lease
  (`../operators/high-availability.md#nats-routes`).
- **Locations are optional.** One location, `default`, exists from the start; location fields stay hidden until a
  second is added.
- **Session expiry follows Vault's model.** The server reports the idle and absolute deadlines; real activity renews
  the idle deadline quietly; a warning with **Stay signed in** appears shortly before it; the absolute maximum cannot
  be extended; tabs share the state; the sign-in page explains an expiry and returns to the same page. Durations are
  administrator settings.

## Consequences
- Pool approval as a separate step, the separate enroll command and the push and preparation scripts go away once
  their replacements exist; until then the current flow keeps working.
- Wiping deletes files but does not erase the disk; terminating the instance is the stronger guarantee and the
  portal says so.
- This supersedes the pool-approval click and pool-bound enrollment described in ADR 0027 and the worker identity
  guide, and the three-file upload in ADR 0044.
- Route verification needs members named by DNS name: a certificate for an IP address carries no DNS name to check,
  so a cluster with an IP-named member keeps verification off and keeps the unwiped-host route risk. A host that
  joins starts without verification until its own route sync has listed every peer. Rolling back to a release without
  the route sync leaves verification on, and members on that release do not add new controllers to their routes.
  The cluster CA still signs a removed host's certificates until they expire; CA rotation was not chosen.

## Implementation notes
- **`samuori-join`, not `samuori join`.** The join entry point is the `samuori-join` script in the release folder, next to
  `samuori-setup`: `sudo ./samuori-join controller <code>` and `sudo ./samuori-join worker <code>`. The `samuori` binary is the
  unprivileged API client (a server URL and an API token, any machine), while joining installs services as root from
  the release folder, so it is not a `samuori` subcommand. Defaults: the host name as `samuori-setup` picks it, from the hostname before cloud metadata (`--host`
  overrides, controllers only), release signers from `./allowed_signers` when present (`--release-signers`
  overrides), and for a worker the whole install and enrolment, with the dedicated-VM confirmation asked
  interactively or given as `--confirm-dedicated-vm`. The long forms (`install.sh ... join --code`,
  `install.sh --worker-only` with `samuori-enroll-worker` and a pasted token) keep working.
- **Codes on the command line.** A code takes the place of the token: a controller code (`samuoric_`) holds the
  controller URL, its certificate pin and a join token; a worker code (`samuoriwj_`) holds the controller worker
  endpoints, the CA pin and an enrollment token. Both tokens are single use, expire within the hour, are stored
  hashed, are shown once and grant nothing until a controller is accepted or a worker is assigned to a pool, so
  taking a code as an argument is acceptable even though it may reach shell history or the process list. The worker
  code is refused when malformed, and `samuori-join worker` checks that a controller presents a certificate for the
  expected name that chains to the pinned CA before installing anything (SEC-WRK-11, TM-115). No code is offered
  when the controller's worker endpoint listens on loopback only.
- **Servers page.** Administration > Servers lists controllers and workers, with join requests (Accept, Reject),
  **Add a controller**, **Add a worker** and **Remove** for both. Upgrades & Diagnostics keeps the controller release
  list and points to Servers; Nodes & workers keeps pools, self-tests and the other worker actions, and its **Enroll a
  worker** shows the same join code.
