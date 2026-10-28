# Practical run: Paketo Buildpacks builds through the worker path

Date: 2026-10-24. Status: **Passed in the dev VM for `rootless-host` with strict-egress settings.** Implements the
`buildpacks` bullet of ADR 0047. Raw output is kept in the maintainer's private records.

## Design checked here

The worker installs nothing for buildpacks. In one attempt's fresh rootless BuildKit daemon it runs two solves:

1. `FROM <run image>@sha256:…` exported as an OCI archive and unpacked (layout file names only) into the attempt
   directory. In layout mode the lifecycle reads the run image from disk and never pulls it (buildpacks RFC 0119).
2. An internal Dockerfile: `FROM <builder>@sha256:…`, the platform directory (`env/BP_*`, `bindings/<name>/type`) and
   the run image layout as local named contexts, the source as the build context, `USER ${CNB_USER_ID}`, then one
   `RUN` of `/cnb/lifecycle/creator -layout -layout-dir=/layout-repo -run-image=<ref>` with each build secret as a
   BuildKit secret mount at `/platform/bindings/<name>/<key>`. The final `scratch` stage holds only the exported
   layout, `report.toml` and the lifecycle's SBOM files, written out with the local exporter.

The worker packs the layout as the image archive (one OCI manifest, blobs verified against their names), so the
canary scan, runtime test, validation, SBOM, signing and publication run as for every other method.

## Setup

- Lima VM from `deploy/dev/lima/samuori-dev.yml` (Ubuntu 26.04 arm64, 4 vCPU, 6 GiB), BuildKit v0.34.0 and RootlessKit
  v3.3.0 from `provision-buildkit.sh`. Nothing else installed for buildpacks.
- Builder `docker.io/paketobuildpacks/ubuntu-noble-builder:0.0.205@sha256:39dd864355d104e1f1b64a4238f6871bf0514ec3fc1427b13544c68452793dd9`
  (image index; linux/amd64 and linux/arm64; lifecycle 0.21.26, platform APIs 0.7–0.15, `CNB_USER_ID=1001`) and run
  image `docker.io/paketobuildpacks/ubuntu-noble-run:0.0.150@sha256:2b7308aea56815c1f376ab84cb1df5338797d480e32d899753e168cd367ab060`
  (index; amd64 and arm64), read from Docker Hub on 2026-10-24. `paketobuildpacks/builder-jammy-base:latest` is a
  single-architecture manifest, so it was not used.
- A `samuori-build` user in `samuori-builds`, the installer's sudo rule for the test user, a shared daemon directory and the
  worker's local egress proxy on the VM address. The nftables confinement was **not** applied, so the run shows that
  the lifecycle uses the proxy, not that the firewall stops a bypass. Images were pulled from Docker Hub directly (no
  mirror).
- `TestBuildpacksWorkerPathVM` (`internal/adapters/isolation/rootlesshost/buildpacks_vm_test.go`), cross-compiled on
  macOS (`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c`) and run as `SAMUORI_CNB_E2E=1 sg samuori-builds -c
  ./rootlesshost.test -test.run TestBuildpacksWorkerPathVM`. It calls `Provider.Prepare`, `Run` and `Destroy` with a
  buildpacks plan for `examples/orders-api`: `BP_NODE_VERSION=22.*`, a bound build secret `npm-token` (an `.npmrc`
  holding a random canary token) as binding `npmrc` of type `npmrc`, and egress `nodejs.org:443,registry.npmjs.org:443`
  (the `nodejs-dist` and `npm` presets). It also attaches the provider to a `workeragent.Agent` and reads the
  capabilities the agent sends to the controller.

## Results

| Check | Observed |
|---|---|
| Capability report | Isolation provider and worker protocol both report `providers=map[buildpacks:0.14]` with nothing installed |
| Detection and build | 5 of 12 buildpacks participated (ca-certificates 3.14.3, node-engine 8.11.0, npm-install 2.3.33, node-start 2.7.11, npm-start 2.5.11); Node 22.23.4 selected from `BP_NODE_VERSION`; `npm ci` ran in the build step |
| Lifecycle user | The step ran as `cnb` (1001:1001); the non-root check passed |
| Experimental feature | Lifecycle 0.21.26 refuses `-layout` unless `CNB_EXPERIMENTAL_MODE` allows it (`experimental features are disabled by CNB_EXPERIMENTAL_MODE=error`); Samuori sets `warn`, and the log says `Platform requested experimental feature 'export to OCI layout format'` |
| Egress | Through the build's proxy only: `nodejs.org:443` (1), `registry.npmjs.org:443` (15). One `registry.npmjs.org` connection in each run was logged as denied with `name did not resolve` or `connection failed` (the proxy's resolver in the VM); npm retried and succeeded |
| Denied egress | With only `registry.npmjs.org` allowed, the proxy denied `nodejs.org:443`, node-engine failed (`Forbidden`), creator exited 51 and the attempt failed as a build failure, not a retry (raw output kept in the maintainer's private records) |
| Secrets | npm-install logged `Loading service binding of type 'npmrc'`; the canary value is not in the log; build-secret canary `passed no delivered secret bytes in 12 layer(s)`, scanning for the proxy credential too. No non-empty `.npmrc` in any layer (npm's own empty `node_modules/npm/.npmrc` is present) |
| Image configuration | `User=1002:1001` (the run image's CNB user), `Entrypoint=[/cnb/process/web]`, no proxy variable or secret in `Env`; default process `web: sh /workspace/start.sh` from npm-start |
| Reproducibility | Image manifest `sha256:d43fabcd013412bcc281adfd128ba246c4564919b5678447202943913089578b` in the prototype and in each of the three worker-path runs that built (one failed only on an earlier, too strict `.npmrc` assertion); image archive SHA-256 `04a67aaee0be941de8b78cc6ab92083afef00874c9c3f43b591fd2c05f7f0521` in every run |
| Lineage | `buildpacks-metadata` (SHA-256 `5284df16…52ff3c`: pins, `io.buildpacks.build.metadata`, lifecycle metadata, report) and `buildpacks-sbom` (`8d30fa9d…94000`: CycloneDX, SPDX and Syft files per buildpack, listing) |
| Duration | 47–49 s per `Run`, including pulls of both images into the fresh daemon |
| Cleanup | After `Destroy` the environment and daemon directories were empty |
| Cgroup scope | The same run with `cgroup-limits=systemd` passed |

## Findings

- Layout export is still experimental in lifecycle 0.21.26; a future lifecycle may change its behaviour. Pinning the
  builder by digest pins the lifecycle.
- The lifecycle records the run image as a path in the layout (`runImage.reference`) and as the builder's tag
  (`runImage.image: …:latest`); the approved digest is recorded by Samuori in the plan and the metadata record.
- The run image layout is BuildKit's re-export of the approved image: same layers, a new manifest. The final image's
  base layers are the approved run image's layers.
- BuildKit expands `${CNB_USER_ID}` from the builder's environment in `COPY --chown` and in secret-mount `uid`, so Samuori
  never needs the builder's user ID in advance.
- The builder's `io.buildpacks.builder.metadata` names its run image by a moving tag; Samuori therefore needs the run image
  pinned separately (provider artifact `run-image`).

## Not covered

- A build submitted through a controller to an enrolled worker (API → plan → dispatch → upload → validation); the
  agent's capability report was checked, not a lease. Done later in [`e2e-providers.md`](e2e-providers.md), with the
  firewall and mirror; its runtime test exposed OD-29.
- `vm-per-job` (the guest path does not run the method yet), Buildah workers (refused), amd64, registry mirrors, the
  worker firewall, a lifecycle cache across builds (none is kept).
- A buildpack that copies a binding into a layer: the canary scan is shown to catch such bytes only in the unit test
  (`TestBuildpacksLayout`).
