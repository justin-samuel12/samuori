# 0032. All-in-one install shapes

Status: **Accepted with conditions** (2026-10-19, owner decision OD-23). Refines the Compose shape in ADR 0012 and ADR 0013.

## Context
ADR 0012 runs the local worker as a separate Compose container, and ADR 0013 runs rootless BuildKit as a separate
Compose service with `seccomp=unconfined`, `apparmor=unconfined` and `systempaths=unconfined` (`compose-rootless`).
The only provider that can pass the worker self-test today, `rootless-dedicated-vm`, needs host facilities that a
container does not have without further weakening: a dedicated build user confined by host nftables rules, systemd
user scopes for per-daemon memory and process limits, and subordinate IDs for RootlessKit. `compose-rootless` also
needs a host-wide user-namespace sysctl on Ubuntu (spike F14), and no provider implements it.

## Decision
- **Native (systemd).** `deploy/systemd/install.sh` installs the controller, worker, egress firewall, registry
  mirror and an optional local OCI registry on one dedicated Linux VM, with checksum-pinned upstream tools. The
  worker runs `rootless-dedicated-vm`.
- **Compose.** `deploy/compose/compose.yml` runs the controller and the local OCI registry. The worker, firewall and
  mirror stay on the host, installed by the same script with `--server-in-compose`. The controller container runs
  read-only with all capabilities dropped and `no-new-privileges`, and uses host networking so its plain-HTTP and
  worker listeners stay on loopback; no Docker socket is mounted.
- `compose-rootless` stays unimplemented. A containerized worker would need either the weakened daemon profile of
  F14 or privileges for nftables and cgroup delegation inside the container.

## Conditions (owner, OD-23)
- One coherent install and upgrade experience for both shapes.
- The host worker is installed, configured, enrolled and managed by the supported setup; users never configure it by
  hand after installing.
- Controller–worker traffic stays on the mutually authenticated worker channel.
- Service dependencies, restart behavior, health checks and recovery are verified.
- No privileged containers and no Docker socket mounts.

## Implementation (2026-10-19)
- `install.sh` installs both shapes and upgrades in place; with `--server-in-compose` it runs `docker compose up -d
  --build --wait` itself.
- The controller hands the host worker a single-use, one-hour enrollment token and its CA pin through
  `/var/lib/samuori-local` (0700); a root pre-start step gives them to the worker, and `samuori-worker.path` starts it. A
  revoked local worker is not re-enrolled automatically.
- Compose health checks use `samuori-server healthcheck` and the registry's `/v2/` endpoint; the controller waits for a
  healthy registry; restart policy `unless-stopped`; the host worker retries until the controller answers.
- Evidence: `docs/operators/all-in-one.md#evidence` (fresh install, upgrade in place and reboot of both shapes).

## Consequences
- Both shapes exercise the real worker protocol, isolation provider and self-test.
- Compose is not a host-independent package: the host still needs the installer's worker components.
- Revisit when a provider can run inside a container without weakening the host, or when `vm-per-job` exists.
