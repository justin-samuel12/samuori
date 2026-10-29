# Build Egress

Status: **Implemented for the `rootless-host` provider (WP15)**; `vm-per-job` applies the same controls inside each
VM once its provider exists (OD-12b). Decision: ADR 0025. Threat rows: TM-42, TM-47, TM-48, TM-49, TM-60.

## What a build can reach

A build step reaches exactly:

- the hosts of the ecosystem presets its recipe needs (or, for Dockerfile builds, the organization's Dockerfile
  presets), through the egress proxy;
- the organization's additional destinations, public or internal, through the egress proxy;
- nothing at all in steps after install: recipe build steps run with `RUN --network=none`.

The build engine pulls base images only from the configured registry mirrors. Everything else is refused: direct
connections, DNS, worker-host services, host loopback and cloud metadata.

## Policy model

| Piece | Where | Notes |
|---|---|---|
| Presets | `internal/egress/presets/*.json`, versioned with releases | npm, Yarn, PyPI, NuGet, PowerShell Gallery, Go proxy, Maven Central, crates.io, Debian, Ubuntu, Alpine, GitHub release downloads, mise and Node.js downloads (Railpack). Each lists `host:port` destinations | A preset may list `methods`; advice suggests it only for those build methods (`mise` is for Railpack only). For a destination it lists, other methods' advice names the preset and the methods it is for, and suggests adding the destination instead.
| Recipe presets | `egressPresets` in each recipe | Node and static sites: npm. Python: PyPI. .NET: NuGet. PowerShell: PowerShell Gallery |
| Organization policy | policy kind `egress` | `allowedPresets`, `dockerfilePresets` and `everyBuildPresets` (both subsets of the allowed ones), `destinations` (`name`, `host`, `port`, `also` for more addresses, `reason`, `network`). Default: every preset allowed; Dockerfile builds get Alpine, Debian and Ubuntu; no destinations |
| Project allowances | `project_egress_allowances` (migration 00061) | A preset or public `host:port` opened for one project's builds of listed methods after an approved request ([below](#project-allowances), ADR 0052) |
| Build plan | `plan.egress` | Preset IDs with versions, the resolved allowlist and the IDs of project allowances it includes (`allowances`). The plan digest covers it |

Admission adds an `egress` policy decision. A recipe that needs a preset the policy does not allow is blocked with
a plain explanation, and the policy dry run lists the specs a change would newly block. Destination hosts are
lower-case DNS names or IP addresses; loopback, link-local (including metadata), multicast and unspecified addresses
are refused. Administrators edit the policy on **Security → Egress** or through `PUT /policies/{id}`. Public and
internal destinations are added separately, each with an optional reason, and stored with a `network`: `public` names must pass the same public-host check as project requests and
resolve only to public addresses at the proxy; `internal` (the default, and what earlier policies hold) may resolve to
private addresses. Like presets, each destination can be switched off (`disabled`) or kept from Dockerfile builds
(`noDockerfile`); both default to off, so earlier policies open every destination for every build method.

### Who can reach a preset

The Network egress page shows one **Who can reach it** choice per preset (owner decision 2026-10-27), stored in the
three lists:

| Choice | Policy | Builds that get it |
|---|---|---|
| No builds | not in `allowedPresets` | none |
| Builds that need it | `allowedPresets` | recipes whose `egressPresets` name it, Railpack and Buildpacks providers configured with it |
| Builds that need it, and Dockerfile builds | also `dockerfilePresets` | the above and every Dockerfile build |
| All builds | also `everyBuildPresets` (and `dockerfilePresets`) | every build of every method |

Destinations offer No builds, All builds except Dockerfile and All builds. A destination may list several addresses
under one name (`also`), sharing its network, reason and switches. Advice and readiness use the same words, so a
blocked GitHub download in a recipe build says to set the GitHub release downloads preset to All builds or add the
destination. "All builds" also lists the preset for Dockerfile builds so controllers older than this field keep that
part during a rolling update; they ignore `everyBuildPresets` and `also` until updated.

### Project allowances

A build's effective egress is the organization policy for its method plus its project's active allowances for that
method (ADR 0052). Admission, readiness, planning and denial advice all use it; an allowed preset adds its
destinations even when the policy does not allow the preset, and an allowed destination is a public rule or, when it
was requested as internal, an internal one (ADR 0052 amendment).

- **Request.** Where a readiness item or a blocked-destination report says an organization administrator can open
  something, a project developer or maintainer can press **Request access** (or `POST
  /projects/{id}/egress-access-requests`) with a justification of at least 20 characters. The request names its
  evidence, never its text: a build, whose stored `egressDenied` destinations the server reads, or a readiness item
  (code, path, line) of a discovery or an admission check, which the server recomputes. Every requested preset or
  destination must be requestable in that evidence. Each destination is requested as public (`destinations`) or
  internal (`internalDestinations`); private addresses and non-public names (single-label, `.internal`, `.local`,
  `.lan`, `.corp`, `.home.arpa` and similar) can only be internal. Duplicate pending requests for the same project, method and
  entries are refused.
- **Refused.** Loopback, link-local (including metadata), multicast and unspecified addresses, as the proxy refuses
  them, whether requested as public or internal.
- **Decision.** An organization administrator other than the requester approves or rejects it under **Approvals**,
  which shows the evidence and exactly what approval opens, for which project and method; internal entries are marked
  as internal network access. Approval creates one
  allowance per entry in the deciding transaction; the decision's `validUntil` becomes the allowance's expiry.
- **Revocation.** Organization administrators revoke an allowance with a reason on the project page (`POST
  /egress-allowances/{id}/revocation`). Builds planned afterwards no longer get it; a run already queued keeps its
  plan. Requests, decisions, allowances and revocations are audited.

### Promoting project allowances

**Security → Egress → Granted to projects** (`GET /egress-allowances/by-destination`) groups the
organization's active destination allowances by destination and network, most projects first. **Add to organization
policy** (`POST /egress-allowances/promotion`, organization administrators, reason of 10 to 1024 characters) adds the
destination to the egress policy for every build method, optionally not Dockerfile builds, and revokes the allowances
it replaces in the same transaction ("Moved into the organization egress policy, version N: <reason>"). Preset
allowances are listed but cannot be promoted. An existing policy entry that is at least as open is kept as it is; a
less open one is a conflict to resolve in the policy (ADR 0052, 2026-10-27 amendment).

## Enforcement on a worker

| Control | Implementation |
|---|---|
| Build user | The BuildKit daemon runs as a dedicated user (`--build-user`) through `sudo -n -u`, never as the worker user. The worker and the build user share a group (`--build-group`, the build user's primary group) used for daemon directories (`--daemon-dir`, mode 2770) and the daemon socket |
| Firewall | `samuori-worker egress-rules` renders an nftables table (`internal/egress/firewall`). Packets from sockets owned by the build user are rejected unless their original destination (`ct original`, before NAT; spike F7) is the proxy or a mirror. An operator applies it as root |
| Proxy | `internal/egress/proxy`, started in-process by `samuori-worker --egress-listen`. Every attempt gets its own credential, passed to steps as BuildKit's predefined `HTTP(S)_PROXY` build arguments, which do not enter image history; user-supplied proxy arguments are dropped. The proxy accepts CONNECT and absolute `http://` requests, matches host and port exactly, resolves names itself and dials only an address that passed the checks: public addresses for preset hosts, project allowances and public destinations, any non-loopback, non-link-local address for internal destinations. It does not intercept TLS. Revoking the credential ends the attempt's tunnels |
| Mirrors | `--mirrors upstream=host:port` writes `buildkitd.toml` registry mirrors. Pulls that fall back to the upstream are refused by the firewall |
| Decisions | Each attempt records up to 1000 decisions; the run log ends with one line per destination and outcome (`egress: allowed …`, `egress: denied … : reason`), and the attempt report carries the denied destinations as structured data ([below](#denied-destinations)) |
| Scheduling | Workers report `strictEgress`. Uploaded source is never assigned to a worker without it (`egress-not-strict`); **Network egress** shows how many connected workers enforce it |

A worker must choose: strict egress (`--egress-listen`, `--build-user`, `--build-group`, `--daemon-dir`) or
`--egress unrestricted`, which is for trusted development hosts only and makes the worker ineligible for uploaded
source.

### Denied destinations

Before a build, the same advice is given for destinations a build is *predicted* to need: the analysis page, the
wizard's **Checks** step and **Check if it can build** list them as build readiness (ADR 0050,
`build-execution.md#build-readiness`). Predictions are static and can be wrong; the denial report below remains the
authority on what a build actually tried to reach.

The build page names the destinations the proxy denied and what an administrator can do about each. Build output is
untrusted and can print its own `egress: denied` lines, so the controller never reads denials from the log:

- The worker's proxy session lists each denied host and port with its decision count, its latest reason and the number
  of decisions beyond the 1000 recorded (`Session.Denied`). On `vm-per-job` the host's proxy session reports them; the
  guest has none.
- The worker sends them in `AttemptReport.egress_denied` and `egress_unrecorded` with the final report (worker protocol
  1.5). The controller keeps at most 50 entries with a valid DNS name or IP address, a port from 1 to 65535 and a
  reason no longer than 200 characters, one per destination, replaces reasons the proxy does not produce, and stores
  them on the attempt (`build_attempts.egress_report`, migration 00059). Older workers send none.
- `GET /builds/{id}/attempts` returns `egressDenied` with advice computed when it is read, from the preset catalog and
  the organization's current egress policy, for the run's build method:
  - the presets that contain the exact host and port;
  - Dockerfile builds: allow the preset for Dockerfile builds (`dockerfilePresets`), allowing it in `allowedPresets`
    first when needed; Railpack and Paketo Buildpacks: add the preset to the provider's egress presets; recipes, or a
    host in no preset: add an egress destination with a reason;
  - a destination the current policy, or a project allowance, already allows: run the build again;
  - otherwise, for a public destination, that a project developer can instead request access for this project only
    (`requestable: true`; [project allowances](#project-allowances));
  - allowed but unresolved or unreachable: no policy change helps;
  - loopback, link-local, multicast and unspecified addresses, and names that resolved to them, cannot be allowed;
    private and reserved addresses get no suggestion either, because a build that probes internal networks should not
    prompt an administrator to open them (`allowable: false`).

The page shows the newest attempt's denials above the attempts, and says that a failed build may have failed because
of them. The log section has a **Copy** button that copies the stored log, or selects it when the browser offers no
clipboard.

### Operator setup (rootless-host)

1. Create the build user with the shared group as its primary group, subordinate IDs and the RootlessKit AppArmor
   profile (`deploy/dev/lima/provision-buildkit.sh` shows the profile).
2. Allow the worker user to run `rootlesskit`, `pkill` and `env` as the build user without a password and without a
   pty (`deploy/dev/lima/provision-egress.sh` shows the sudoers entry). This lets the worker run any command as the
   unprivileged build user, which is the intended boundary: the firewall confines that user, not the worker.
3. Run a registry mirror for each upstream (CNCF Distribution in proxy mode).
4. Apply `samuori-worker egress-rules --build-user <user> --allow <proxy ip:port>,<mirror ip:port> | nft -f -`.
5. Start the worker with the strict-egress flags above.

### Mirrors for other registries

The installers set up docker.io (`10.231.0.1:5000`) and, with `--railpack`, ghcr.io (`10.231.0.1:5002`). For any
other upstream, such as `mcr.microsoft.com` after a base-image request is approved, pass
`--registry-mirror <host>` to `install.sh` on each worker host (repeatable). Each host gets:

- a CNCF Distribution pull-through config `/etc/samuori/mirror-<host>.yml` with `remoteurl: https://<host>`, data in
  `/var/lib/samuori-registry/mirror-<host>`, served by the templated unit `samuori-mirror@<host>.service`;
- the next free port from `10.231.0.1:5003`, kept in `/etc/samuori/registry-mirrors` so reruns keep it;
- an entry in the worker's `--mirrors` list and the firewall allowlist.

The option is recorded in `/etc/samuori/install-args`, so self-updates keep it. Running the installer without a host that
was mirrored before stops its unit and removes its config (the cached layers stay until uninstall). The mirrors pull
anonymously; registries that need credentials use image sources (`registry-auth.md`).

## Evidence

> History: the tests and make targets named below ran at earlier commits and were removed (ADR 0039); restored
> focused tests are marked in `docs/developers/security-test-catalog.md`.

`make test-buildkit` in the Linux dev VM (after `provision-egress.sh`) runs real builds as the build user behind
the firewall, proxy and a docker.io pull-through mirror:

- a probe in the install step finds direct internet, DNS (`resolve4` and `getaddrinfo`), a worker-host service, host
  loopback and the metadata address blocked;
- through the proxy, the allowlisted host gets 200, another host 403, and a request without the credential 407;
- the following `--network=none` step cannot reach the proxy;
- `npm install` succeeds through the proxy, and the base image arrives through the mirror;
- a base image from a registry without a mirror fails at the network;
- `--registry-mirror` is checked by `bash -n`, a test that the option is recorded in and replayed from
  `install-args`, and a local run of the port-assignment snippet; it has not run on a real worker host yet;
- a restarted provider finds an orphaned environment, kills its build-user daemon and removes its state.

Unit tests cover the proxy (credentials, allowlist, address checks, DNS rebinding, session isolation, revocation,
plain-HTTP forwarding), the policy, plan resolution, admission and scheduling.

## Limitations and pending work

- The proxy runs inside `samuori-worker` rather than as a separate process; see OD-21.
- Host-level filtering cannot stop exfiltration to an allowed host (ADR 0025).
- The PowerShell Gallery preset may need CDN hosts that are not listed; denied hosts appear on the build page and in
  the run log.
- Denial advice reflects the current policy, not the one the build ran under, and has been exercised by focused tests
  only, not yet with a real denied build on a worker or in a browser.
- Mirror authentication with build-scoped tokens (OD-14), mirror health on the admin Overview, and mirror garbage
  collection are not implemented.
- SEC-NET-02 (metadata on a cloud worker) needs a cloud worker; the dev VM has no metadata service (spike F16).
- `vm-per-job` egress needs the KVM provider (OD-12b).
