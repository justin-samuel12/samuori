# 0013. BuildKit default engine; standalone requirements; macOS development

Status: Accepted (2026-10-19). Isolation profiles approved by the owner (OD-4, 2026-10-19); providers and lifecycle in
ADR 0027.

## Context
BuildKit is the default engine (D05). Simple installation (no Kubernetes or external services) still needs a build
engine, and the primary developer workstation is macOS (Apple Silicon), where BuildKit cannot run natively.

## Decision

### BuildKit is a required component of every worker
Pinned version: BuildKit v0.34.0 (Apache-2.0), rootless mode by default.

Host requirements for rootless BuildKit (from upstream `docs/rootless.md`; verify in the spike):
- Linux with user namespaces enabled (`user.max_user_namespaces` > 0; `kernel.unprivileged_userns_clone=1` on
  Debian-family kernels; `kernel.apparmor_restrict_unprivileged_userns=0` on Ubuntu 23.10+).
- `newuidmap`/`newgidmap` (uidmap package) and `/etc/subuid`, `/etc/subgid` entries for the build user.
- RootlessKit (bundled in the rootless image and release archive).
- Kernel ≥ 5.11 for native rootless overlayfs (fuse-overlayfs with `/dev/fuse` from 4.18).
- cgroup v2 with systemd delegation for CPU/memory/pids limits.

### Standalone deployment shapes
- **Native Linux (systemd):** `samuori-worker` and rootless `buildkitd` run as a dedicated unprivileged user;
  `buildkitd` listens on a unix socket readable only by that user.
- **Docker Compose:** `moby/buildkit:v0.34.0-rootless` as a separate service, reached only by the `samuori-worker`
  service over a private socket volume. Upstream requires `seccomp=unconfined`, `apparmor=unconfined` and
  `systempaths=unconfined` for rootless BuildKit in a container. This relaxation must be analyzed in the threat model;
  `--oci-worker-no-process-sandbox` is not used. No host Docker socket is mounted anywhere.
- **Isolation profiles** (evidence: `docs/spikes/buildkit-isolation.md`). Rootless BuildKit alone is **not**
  treated as sufficient isolation for untrusted source, because build steps share the host kernel. Each worker
  pool declares exactly one profile:
  - `vm-per-job`: default for untrusted builds (approved).
  - `rootless-dedicated-vm`: alternative where policy permits; recycled regularly (approved).
  - `rootless-host`: explicitly trusted workloads only (approved).
  - `compose-rootless`: evaluation installs in a disposable VM.
  - `kata`: candidate, pending spike part 2.
  - `gvisor`: not supported.
  Every profile adds strict egress (ADR 0025), daemon-level resource limits and a separate daemon and cache per
  trust domain.
- **Security invariants for every profile (OD-16 to OD-18, 2026-10-19; evidence `docs/spikes/buildkit-secrets.md`):**
  - **One active build per `buildkitd`**, always. This is a security invariant, not a performance setting, and is
    not configurable. The scheduler never assigns a second build to an occupied daemon. The worker independently
    refuses one. Session identifiers are not relied on to separate builds.
  - **A fresh `buildkitd` for every secret-bearing build**, terminated afterwards. A daemon that has handled
    build-time secrets is never reused.
  - **Cache isolation for secret-bearing builds** follows `docs/architecture/build-cache.md`: no reuse until OD-19
    validates scoped controls.
- **Host prerequisites learned from the spike:** on Ubuntu 23.10+, grant `userns` to RootlessKit through a
  per-binary AppArmor profile, never by disabling `kernel.apparmor_restrict_unprivileged_userns` host-wide. The
  exception is a disposable VM running `compose-rootless`, where the VM is the boundary.
- **Isolation profile gate (OD-4, approved 2026-10-19):** every uploaded source is untrusted. A build of uploaded
  source runs only on a pool whose profile is approved (above) **and verified** by the pool's automated self-test
  (ADR 0027). Until such a pool exists, uploaded-source builds stay disabled and builds run only in spikes and tests
  against synthetic fixtures. The scheduler enforces this.
- Preflight checks in first-run setup verify each requirement and report actionable failures.

### macOS development
- `samuori-server` and the UI dev server run natively on macOS.
- Builds run in a Lima VM (Apache-2.0) defined in `deploy/dev/lima/samuori-dev.yml`: Ubuntu LTS, rootless BuildKit,
  and `samuori-worker`, which connects outbound to the host controller.
- Docker Desktop or Colima are not required and are never given to Samuori via a socket.
- Apple Silicon builds `linux/arm64` natively; `linux/amd64` uses QEMU emulation and is slow. Integration tests run
  the native architecture by default.

### Windows (OD-7, owner-approved deferral 2026-10-19)
Windows container execution is deferred until a feasibility experiment proves engine capability and isolation. The
support matrix lists Windows as unsupported until then. The architecture stays platform-aware so Windows can be
added without redesign:
- Platforms are modeled as OCI platform tuples (`os`, `architecture`, `variant`, `os.version`), never assumed to be
  Linux.
- Worker capabilities advertise OS, architecture, engine and isolation profiles; scheduling matches on them.
- Recipes, base-image policies and validation profiles declare their supported platforms.
- Discovery keeps reporting Windows-only evidence (e.g. PowerShell) as a constraint, and admission rejects Windows
  targets with an actionable "not yet supported" message.

## Consequences
"No external services" for all-in-one means no Kubernetes, registry, database or message server. A Linux kernel with
the features above and the bundled BuildKit are still required, and the install docs say so.
