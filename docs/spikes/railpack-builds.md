# Practical run: Railpack builds through the worker path

Date: 2026-10-24. Status: **Passed in the dev VM for `rootless-host` with strict-egress settings.** Follows the
planning spike ([`railpack.md`](railpack.md)); implements ADR 0047. Raw output is kept in the
maintainer's private records.

## Setup

- Lima VM from `deploy/dev/lima/samuori-dev.yml` (Ubuntu 26.04 arm64, 4 vCPU, 6 GiB), BuildKit v0.34.0 and RootlessKit
  v3.3.0 from `provision-buildkit.sh`.
- Railpack v0.43.0 `railpack-v0.43.0-arm64-unknown-linux-musl.tar.gz`, archive SHA-256
  `732ece14…a42ac9` matching upstream `checksums.txt`; installed as `install.sh --railpack-archive` does into
  `/usr/local/lib/samuori/providers/railpack/`; binary SHA-256 `d70e585b…b042f`.
- A `samuori-build` user in `samuori-builds` with the installer's sudo rule, a daemon directory shared with that group, and
  the worker's local egress proxy listening on the VM address. The nftables confinement was **not** applied, so the
  run shows that planning and steps use the proxy, not that the firewall stops a bypass.
- `TestRailpackWorkerPathVM` (`internal/adapters/isolation/rootlesshost/railpack_vm_test.go`), cross-compiled on
  macOS and run as `SAMUORI_RAILPACK_E2E=1 sg samuori-builds -c ./rootlesshost.test -test.run TestRailpackWorkerPathVM`. It
  calls `Provider.Prepare`, `Run` and `Destroy` with a railpack plan for `examples/orders-api`: pinned binary,
  frontend `ghcr.io/railwayapp/railpack-frontend:v0.43.0@sha256:355ef61f…76cc`, builder and runtime images by
  digest, a bound build secret `npm-token` and `buildCommand: printenv NPM_TOKEN >/dev/null && echo
  npm-token-present`.

## Results

| Check | Observed |
|---|---|
| Capability report | `providers=map[railpack:v0.43.0]` once installed |
| Planning | `railpack prepare` ran as `samuori-build` inside RootlessKit with the attempt's own `/tmp/railpack`; `/tmp/railpack` on the host stayed empty |
| Egress | Through the build's proxy only: `github.com`, `release-assets.githubusercontent.com`, `mise-versions.jdx.dev`, `mise.jdx.dev` (planning and `mise install`), `nodejs.org`, `registry.npmjs.org` (15 requests), `deb.debian.org:80` (7) |
| Denied egress | With only `registry.npmjs.org` allowed, the proxy denied `github.com:443`, Railpack exited 75, and Samuori reported a build failure naming the egress denial instead of retrying (raw output kept in the maintainer's private records) |
| Gateway build | BuildKit `gateway.v0` resolved the digest-pinned frontend; builder and runtime images were pulled by digest; 28–37 s per run |
| Secrets | The step printed `npm-token-present`; the value is not in the log; build-secret canary `passed no delivered secret bytes in 11 layer(s)` |
| Image configuration | No proxy variable and no secret in `Env`; `Cmd=[npm run start]`, `Entrypoint=[/bin/bash -c]`, `User=""` (root) |
| Lineage | `railpack-plan.json` (SHA-256 `5e3dec01…9663e`) and `railpack-info.json` (`dfffe6bf…92b60`) returned as records |
| Cleanup | After `Destroy` the environment and daemon directories were empty, including files `samuori-build` wrote in its user namespace |
| Cgroup scope | The same run with `cgroup-limits=systemd` passed |

Image archive SHA-256 of the recorded run: `5d6d408dd72daec234957a05ac3f5160147d0c1081bccb71ebf86e33310d732b`.
Railpack sets `RAILPACK_BUILT_AT`, so each build's image differs.

## Findings

- Railpack's frontend reads only `cache-key`, `secrets-hash` and `github-token` build arguments and ignores the
  others, including BuildKit's proxy arguments; step `variables` propagate into the image's `Env`. Samuori therefore
  delivers the egress proxy as BuildKit secrets on every step that runs commands.
- Railpack keeps mise in the fixed directory `/tmp/railpack/mise` and ignores `TMPDIR`; a bind mount in a private
  mount namespace gives each attempt its own.
- `--env NAME` without a value does not declare a secret; Samuori writes the plan's `secrets` list itself.
- Railpack v0.43.0 downloads mise from GitHub releases without verifying a checksum (`core/mise/install.go`).
- Railpack images run as root by default. Since 2026-10-26 Samuori adds a non-root user layer unless the provider keeps
  root (ADR 0047 amendment); this spike's image predates it.

## Not covered

- A build submitted through a controller to an enrolled worker (API → plan → dispatch → upload → validation): done
  later in [`e2e-providers.md`](e2e-providers.md), which found that planning ignored the proxy behind the firewall
  (mise runs without the proxy variables) and that ghcr.io needed a mirror; both fixed there.
- `vm-per-job`: the guest image does not carry Railpack.
- amd64; a staged, digest-verified mise cache; the worker firewall in this run.
