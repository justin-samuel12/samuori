# Spike: Removing a NATS Member with a System Account

Date: 2026-10-22, route verification 2026-10-23. Status: **Complete for the in-process case; not yet run on separate hosts.** Code:
`internal/clusterjoin/natssystem_test.go` (`TestNATSMemberRemoval`, `TestSystemAccountUpgradeIdempotent`).

## Question
Controller removal (ADR 0045) must drop the departed controller's NATS member from the JetStream meta group, which
only the system account may do (`$JS.API.SERVER.REMOVE`). The existing members authenticate controllers by client
certificate alone and keep Samuori's stream in the global account. Can a system account be added to that configuration,
on new installs and by rewriting existing `nats.conf` files, without moving the stream or changing how controllers
connect?

## Setup
- Embedded `nats-server` v2.15.1 and `nats.go` v1.54.0, three servers in one test process on macOS arm64, routes and
  clients on 127.0.0.1 with mutual TLS from a cluster CA created by `clusterjoin.Create`.
- Each server parses the configuration Samuori generates (`Bundle.natsConfig` plus `EnsureSystemAccount`), with only
  ports, server names and the store directory overridden for the test.
- The system account file holds `accounts { SYS { users: [ { user: "samuori-system", password: "<bcrypt>" } ] } }`,
  `system_account: SYS`, a password-less default user in the global account and `no_auth_user` naming it.

## Results

| Step | Outcome |
|---|---|
| Parse the upgraded configuration | `SystemAccount = SYS`, `NoAuthUser = samuori-controller` |
| Controller connects with its certificate only | Accepted as the default user; JetStream available in the global account (the system account does not count as another account, so `$G` keeps JetStream) |
| Create an R3 stream | Succeeds once the meta group forms |
| System user, wrong password | Refused |
| System user, right password, no client certificate | Refused (TLS `verify: true`) |
| Stop `n3`, then `$JS.API.SERVER.REMOVE {"peer":"n3"}` | Success; both remaining servers report a meta group of 2 (was 3) |
| Same request again | Error 10044 (not a member), treated as done |
| Lower the stream to R2 and publish | Succeeds |
| Drop `n3` from the routes and reload | `DropRoute` rewrites only that route; `ReloadOptions` with the new routes succeeds |
| Run the upgrade twice on an old `nats.conf` | First run changes it; the second changes no byte of any file; the password is 0600 and appears in no NATS file |

Four repeated runs passed (about 4.5 s each).

## Conclusions for the implementation
- The system account can be added without moving the stream: controllers stay in the global account through
  `no_auth_user`, and mutual TLS still gates every connection.
- `nats-server` cannot change the system account on reload, so the installer restarts `samuori-nats` after adding it;
  installs and updates already restart it.
- Removal must follow the wipe: the request is answered only by the meta leader, after the change commits. With a
  member stopped, the remaining majority commits it.
- `JetStreamClusterPeers` reports only online peers on the leader, so it cannot show a removal; `Jsz().Meta.Size`
  does.

## Three Lima VMs (2026-10-22)
`deploy/dev/lima/cluster.sh` on three controllers (PostgreSQL 18, blobs in a TLS registry): joins with acceptance,
removing a follower (wiped, routes and meta group shrunk through the path-unit helper and `SIGHUP`), refusing one of
the last two, rejoining the wiped host, and removing the leader. It found:

- A rejoined host was forgotten again by a member that restarted before the rejoined controller started; accepting
  the join now retires the removal (`rejoined`).
- NATS ignores a peer-removed server name for five minutes (`peerRemoveTimeout` in nats-server 2.15.1's `raft.go`),
  so a host that rejoins sooner is connected but not in the meta group. Removing the leader then left one of two
  meta peers and stalled JetStream for good. Removal now also requires the stream's current replicas, read from
  JetStream, to keep a majority without the departing host.
- A restrictive umask left `/etc/samuori/nats` unreadable to the `nats` user; the installer now sets directory modes.

## Route verification (OD-27, 2026-10-23)
Owner decision OD-27, option 1: routes list exactly the members and route TLS uses
`verify_cert_and_check_known_urls`. Code: `internal/clusterjoin/routes_test.go` (`TestNATSRouteVerification`,
`TestPlanRoutes`, `TestSetRoutesRoundTrip`), `cmd/samuori-server/routes_test.go`, `internal/cluster/routes_test.go`.

**Source reading (nats-server v2.15.1).** `isRouterAuthorized` (`server/auth.go`) accepts an incoming route, when
`Cluster.TLSCheckKnownURLs` is set, only if a DNS SAN of the peer's certificate matches the host of one of
`opts.Routes`; IP SANs are never checked. Routes this server opened itself are not checked. On reload
(`server/reload.go`), a `routes` change closes the solicited routes that left the list and starts the new ones,
and a `cluster` change counts as an authorization change, so `reloadAuthorization` re-checks every accepted route
against the new options and closes the ones that fail. `SIGHUP` runs `Server.Reload`, which parses the file and calls
`ReloadOptions`; the test calls `Reload` on servers started from their files, the same path. `varz.config_load_time`
moves on each successful reload, which the helper uses to confirm it.

**Setup.** Four embedded servers in one process (macOS arm64, Go 1.27.2), each started from the file Samuori generates
(`Bundle.natsConfig` plus the system account) with only ports and the store directory replaced. Names
`n1.localhost` .. `n4.localhost` (they resolve to 127.0.0.1 here; the test skips where they do not), certificates
from `clusterjoin.Create` with those DNS names, routes and verification written by `SetRoutes` and planned by
`PlanRoutes` with the peers read through the system account (`LocalState`, `$SYS.REQ.SERVER.<id>.VARZ|ROUTEZ`).

| Step | Outcome |
|---|---|
| n1..n3 with verification and full routes | Full mesh |
| n1 and n2 sync a plan with n3 removed, then reload | n3, still running with its valid certificate and routes to n1 and n2, has no route; it stays at none for 5 s while it retries; n1–n2 stay routed |
| Negative control: verification off on n2 only | n3 routes back to n2 at once; in 4 of 7 runs n2's gossip also made n1 open an implicit route to n3 |
| Verification on again on n2 | n2 drops n3; an implicit route n1 opened survives the reload, as the source says (solicited routes are not checked) |
| Restart n1 | The implicit route is gone; n3 has no route for 3 s |
| n4 starts with its bundle's routes (n1, n4) and no verification | No route for 4 s: n1 refuses it, nobody else dials it |
| n1 and n2 sync a plan with n4 | n4 routes to n1 and n2 within 15 s |
| n4's own sync | Fills n2, turns verification on (every connected peer listed); n3 still has no route |

Seven runs passed (about 20 s each). `TestSetRoutesRoundTrip`: the rewritten file parses with `TLSCheckKnownURLs`,
only the cluster TLS block changes, rewriting is byte-for-byte idempotent and `DropRoute` (cluster forget) still
reads it. `TestSyncRoutesHelper` drives the helper against a fake member: a reload that does not take restores the
file, the same plan twice reloads once, a surviving route to a removed member is reported and restarts the member
only when allowed, a plan that cuts the member off is refused, a stopped member gets the file only.

**Reliability (2026-10-26).** Under load the spike failed about one run in ten with "n3 is routed to [n2]" or "n4 is
routed to [n1]". The cause was the measurement, not a route: a member that dials a peer registers the route as soon
as the peer's INFO arrives, before the peer checks its certificate and refuses it, so the removed member's own
ROUTEZ lists a peer for a moment on every retry. The spike now counts a route only when both ends list it, and checks
a refused route on the accepting side. A second, real effect: after n2 turns verification on again, the implicit
route n1 opened lets n1 gossip n3 back to n2, which then dials n3 itself; only a restart drops that, so the spike
repeats the controller's follow-up checks (restart a member still routed to n3 after 2 s, then require 3–5 s with no
route, within a minute) instead of a fixed number of rounds. The controller now runs those checks 5 s after a plan
change, a controller start or stop, or a restart, doubling to 160 s (`TestNATSRouteFollowUp`), rather than only every
ten minutes. With `-race` on macOS arm64: before the change 3 of 20 runs under load failed (two on a one-sided route, one on a system-account connection that timed out); after it 32 of 32 passed,
20 while race-enabled tests of five other package trees ran and 12 while `make ci` ran. One intermediate run with two
copies of the spike in parallel was refused by a member's system account, probably a port another copy reused; the
spike now retries that connection for 15 s.

**Conclusions.**
- Rewriting routes and reloading is enough to refuse a removed member; no restart is needed for that.
- A member that joins with partial routes and verification on refuses peers it does not list yet, so a new member
  starts without verification and turns it on in its own sync.
- Gossip can open a route the reload cannot close; the helper detects it and the controller restarts that one member
  under a cluster lease.
- Verification needs DNS names: members named by IP address stay unverified.

## Not covered
- Separate hosts, and route verification on VMs or AWS.
- Removing a member that is still running.
- The systemd path unit and `systemctl kill -s HUP` path end to end (exercised in-process only).
