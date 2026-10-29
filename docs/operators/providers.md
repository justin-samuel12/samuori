# Build and conversion providers

Optional open-source tools Samuori runs on workers, inside build isolation, never on a controller or during discovery
(ADR 0047, ADR 0048). Build supply > **Build & conversion providers**.

| Provider | Used for | Pinned artifacts |
|---|---|---|
| Railpack | the `railpack` build method | the `railpack` binary per architecture (SHA-256); `ghcr.io/railwayapp/railpack-frontend`, `railpack-builder` and `railpack-runtime` by digest, same version |
| Paketo Buildpacks | the `buildpacks` build method | a CNB builder image and the run image it names, by digest; nothing installed on workers |
| Kompose | Compose → Kubernetes conversion | the `kompose` binary per architecture |
| Move2Kube | source, Dockerfile and Compose → Kubernetes, Helm or Kustomize | the `move2kube` binary per architecture; unmaintained since 2025-03, off by default |

- **Off until approved.** Enabling a provider needs an approved version and every artifact pinned. Workers report the
  providers they have installed; the page shows how many online workers have the approved version. A provider runs
  only on such a worker.
- **Scope.** Restrict a provider to worker pools or projects; none selected means all. Egress presets name what the
  provider may reach while planning or building; the build's egress policy still applies on top.
- **Build methods.** Choose which methods BuildSpecs may use (`recipe`, `dockerfile`, `railpack`, `buildpacks`) and
  the default for new BuildSpecs; *Automatic* recommends one from discovery. Admission refuses a BuildSpec whose
  method is not allowed (`build-method-not-allowed`) or whose provider is off for its project. Existing BuildSpecs
  keep their method. The default policy allows `recipe` and `dockerfile`.
- Changes are audited (`provider.configure`, `build-method-policy.set`). API: `GET /api/v1/providers`,
  `PUT /api/v1/providers/{name}`, `PUT /api/v1/build-method-policy`.

## Railpack

1. **Install it on the workers that should run it.** `sudo ./install.sh ... --railpack` downloads Railpack v0.43.0
   from its GitHub release and checks it against the upstream checksum pinned in the script; `--railpack-archive
   <file>` installs an archive you downloaded instead, checked the same way. It lands in
   `/usr/local/lib/samuori/providers/railpack/` (`railpack` and `VERSION`), the `railpack-dir` setting of the
   `rootless-host` and `rootless-dedicated-vm` providers. The same flag installs `mise` 2026.10.14 there, the version
   Railpack v0.43.0 plans with, checked against mise's `SHASUMS256.txt` (`--mise-archive <file>` for a downloaded
   `mise-v2026.10.14-linux-<arch>-musl.tar.gz`): Railpack runs mise without the proxy variables, so with strict egress
   the worker runs this verified copy through the build's proxy instead, and refuses to plan without it. On an
   all-in-one or worker host, `--railpack` also runs `samuori-mirror-ghcr`, a ghcr.io pull-through mirror on
   `<bridge>:5002`, and allows it in the build-user firewall, because the Railpack images come from ghcr.io. The flag
   is kept for self-updates. Installing changes the worker's configuration digest, so the worker self-tests again.
   Basic installs skip this.
2. **Approve it.** Enable Railpack with version `v0.43.0` and pin:

   | Artifact | v0.43.0 value |
   |---|---|
   | `binary-arm64` | `sha256:d70e585b1cc0c9782fe2b09b3b84c1528833cc532d3cb0766201b8feb32b042f` (the installer prints the value for its host) |
   | `binary-amd64` | `sha256:3e9a8477b2c7cd0bfb46ebdb0ca5f3153329f4f01cec263dbb6ea9b4679a3af9` (binary extracted from the checksum-verified x86_64 archive; not run on amd64) |
   | `frontend` | `ghcr.io/railwayapp/railpack-frontend:v0.43.0@sha256:355ef61fae6f2551103cd8b10c710cb07167dd353d561f15c69b8d7ac1c676cc` |
   | `builder` | `ghcr.io/railwayapp/railpack-builder:mise-2026.10.14@sha256:03df9e9aa1e786c2e0dd12c5409acad3bbd5a0fa8a75c1117c7ddab5c0c3fde4` |
   | `runtime` | `ghcr.io/railwayapp/railpack-runtime:mise-2026.10.14@sha256:02820533713338569ae4d4f0df986c3e7cc6e93457fd06fbb0d2cfa86fba9cdb` |

   Digests are the image indexes read from ghcr.io on 2026-10-24; check them yourself before approving.
3. **Allow its destinations.** Egress presets for a Node project: `mise` (mise and its tool downloads), `nodejs-dist`,
   `npm`, `debian` (the runtime image installs packages). The organization's egress policy must allow each, and with
   strict egress the registry mirrors must serve `ghcr.io` (`samuori-mirror-ghcr` above; set your own with the worker's
   `--mirrors ghcr.io=<host:port>`). Add `ghcr.io/railwayapp/railpack-builder` and
   `ghcr.io/railwayapp/railpack-runtime` to the base-image policy, or admission blocks the build.
4. **Allow the method** in the build method policy, optionally restricted to pools and projects.

Railpack plans on the worker as the build user, inside a user and mount namespace, through the build's egress proxy,
and never on a controller or in discovery (`../architecture/build-execution.md#railpack-builds-adr-0047`). Only
workers reporting the approved version receive Railpack builds; the binary's checksum is checked again before every
run. `vm-per-job` guest images do not carry Railpack yet, so use `rootless-dedicated-vm` pools. Railpack images run
as 10001:10001: the worker adds a final layer that gives that user the image's working directory and the
directories below it (files stay root-owned) and sets the image user. To keep Railpack's root user for an
organization, select **Keep Railpack's root user** when configuring Railpack (`runAsRoot: true` in
`PUT /providers/railpack`); new builds then run as root, already-built images are unchanged. Railpack images set
`RAILPACK_BUILT_AT`, so rebuilds differ.

Status: the registry, policy, admission, dispatch and worker checks are covered by focused tests (SEC-RPK-01..07);
a real Railpack build passed through the worker's isolation provider in the dev VM
(`../spikes/railpack-builds.md`), and through a controller and its enrolled worker on an all-in-one VM with the
firewall, mirrors and validation (`../spikes/e2e-providers.md`, 2026-10-24, arm64).

## Paketo Buildpacks

1. **Nothing to install.** Every BuildKit worker of a `rootless-host` or `rootless-dedicated-vm` pool can run the
   CNB lifecycle, which comes with the builder image; workers report `buildpacks 0.14` (the CNB platform API Samuori
   drives), and the page counts the online workers that can run it. Buildah and `vm-per-job` workers cannot yet.
2. **Approve a builder and its run image**, both pinned by digest, for every architecture your workers have. Read the
   builder's `io.buildpacks.builder.metadata` label: `lifecycle.version` is the version to approve, `lifecycle.apis.
   platform.supported` must include `0.14`, and `images[0].image` names the run image, usually by a moving tag; pin
   the matching run image release by digest. Values checked on 2026-10-24 (image indexes for linux/amd64 and
   linux/arm64):

   | Artifact | Value |
   |---|---|
   | version | `v0.21.26` (the builder's lifecycle) |
   | `builder` | `docker.io/paketobuildpacks/ubuntu-noble-builder:0.0.205@sha256:39dd864355d104e1f1b64a4238f6871bf0514ec3fc1427b13544c68452793dd9` |
   | `run-image` | `docker.io/paketobuildpacks/ubuntu-noble-run:0.0.150@sha256:2b7308aea56815c1f376ab84cb1df5338797d480e32d899753e168cd367ab060` |

   Check them yourself before approving, for example with `crane digest` or your registry's UI.
   `paketobuildpacks/builder-jammy-base:latest` is amd64-only. The time limit bounds the lifecycle step (default
   1800 s).
3. **Allow its destinations.** For a Node project: `nodejs-dist` and `npm`. Other languages need their own presets
   (`pypi`, `maven-central`, `go-proxy`, …), and buildpacks that download from other hosts need internal
   destinations. Add `docker.io/paketobuildpacks/ubuntu-noble-builder` and `docker.io/paketobuildpacks/ubuntu-noble-run`
   to the base-image policy, or admission blocks the build. With strict egress the registry mirrors must serve them.
4. **Allow the method** in the build method policy, optionally restricted to pools and projects.

Developers set buildpack options under `spec.build.buildpacks.env` (`BP_*`, `BPE_*`, `BPL_*`; never secrets) and give
each build secret a CNB binding, such as type `npmrc` with key `.npmrc` for an npm token. The lifecycle runs as the
builder's non-root user, through the build's egress proxy (`../architecture/build-execution.md#buildpacks-builds-adr-0047`).
Samuori exports the image as an OCI layout because the lifecycle's other exports need registry push credentials or a
Docker daemon inside the build, which Samuori never provides. Layout export is experimental upstream in lifecycle 0.21
(Samuori sets `CNB_EXPERIMENTAL_MODE=warn`), so a new builder may change or drop it. The lifecycle comes inside the
pinned builder, so nothing changes until you approve a new builder: run a test build with it first. A broken export
fails the build at the lifecycle step or Samuori's blob digest check; it cannot produce an unchecked image. No cache is
kept between buildpacks builds yet.

**Known limitation (OD-29):** Samuori's runtime test runs without `/sys`, and Paketo's Node.js images run a launch
helper that exits when it cannot read cgroup memory limits, so their runtime test fails (`exited with status 82`)
although they start under Docker or Kubernetes. OD-29 is decided (a read-only view of the test's own cgroup), but
rootless BuildKit cannot provide it safely (`../spikes/runtime-test-cgroup-view.md`); until another runtime path is
approved, Node.js projects add
`BPE_DEFAULT_MEMORY_AVAILABLE: max` to `spec.build.buildpacks.env`, which makes the helper skip the lookup and changes
nothing else unless `OPTIMIZE_MEMORY` is set.

Status: focused tests cover the policy, admission, plan, dispatch and worker checks (SEC-CNB-01..07); a real Paketo
build passed through the worker's isolation provider in the dev VM (`../spikes/buildpacks-builds.md`), and through a
controller and its enrolled worker on an all-in-one VM, validation included with the setting above
(`../spikes/e2e-providers.md`, 2026-10-24, arm64).

## Kompose (Kubernetes conversions)

1. Install the tool on each worker that should convert: `sudo ./install.sh --release <dir> ... --with-kompose`
   downloads Kompose v1.38.0 for the host's architecture, checks it against the release's SHA-256 and writes
   `/usr/local/lib/samuori/providers/kompose/kompose` and `VERSION` (`v1.38.0`). To install by hand, put the binary and a
   `VERSION` file there yourself; `samuori-worker --providers-dir` changes the folder. Basic installs need none of this.
2. Restart the worker. It reports `kompose v1.38.0` only when its isolation provider can run conversions
   (`rootless-dedicated-vm`, or `rootless-host` for trusted pools, with BuildKit and the static `samuori-worker` as tool
   runner); `vm-per-job` workers do not yet run conversions.
3. Under **Build & conversion providers**, enable Kompose with version `v1.38.0` and the SHA-256 of each
   architecture's binary, from the release's `SHA256_SUM`:
   `sha256:65a6a720605bead3964e8b22d423a0763de451a236fe03de902e366cf3d9c147` (linux/amd64),
   `sha256:24faf6212ec34d325e4724118098d34b9dd321cda2d9ca22efa0f04cdb909894` (linux/arm64). The time limit (default 300
   seconds) bounds each run.

A conversion runs only on a worker that reports the approved version, in an allowed pool, whose isolation is
verified for uploaded source; the worker checks the binary against the approved SHA-256 before running it and runs
it with no network and no credentials (`../architecture/kubernetes-conversion.md`). Developers then use **Generate
Kubernetes configuration** (in-app Help: *Generate Kubernetes configuration*).

## Move2Kube (Kubernetes conversions from source)

**Maintenance:** Move2Kube's last release, v0.3.15, is from March 2025 and its last commit from November 2024 (checked 2026-10-24). It
stays off until you enable it, and the wizard shows this warning to developers. Enable it only after weighing that
an unmaintained tool parses uploaded source; it runs with no network, no credentials and no container runtime, and
Samuori validates everything it writes, but parser bugs will not be fixed upstream. Kompose remains the choice for
Compose files.

1. Install it on each worker that should convert: `sudo ./install.sh --release <dir> ... --with-move2kube` downloads
   `move2kube-v0.3.15-linux-<arch>.tar.gz`, checks it against the release's `.sha256sum`
   (`5bcda9f36be300a5b161b56e93bb4ca93920fa6e16ebaf34453f0778dddb5c0f` amd64,
   `d2a28dea9886c6d3110870be321fe69f1f5d9335b9031504ca4003e5265977bd` arm64), installs
   `/usr/local/lib/samuori/providers/move2kube/move2kube` and `VERSION` (`v0.3.15`), and prints the binary's SHA-256 to
   approve. By hand: extract `move2kube/move2kube` from the same archive into that folder and write `VERSION`.
2. Restart the worker; it reports `move2kube v0.3.15` under the same conditions as Kompose.
3. Under **Build & conversion providers**, enable Move2Kube with version `v0.3.15` and the SHA-256 of each
   architecture's binary (the binary, not the archive):
   `sha256:7f63f3ab9cd04ba9d887a59ff68d409612da26274e7c44d3efb6dbaec0afb501` (linux/amd64),
   `sha256:2edf77acd01ff02e3e54fa72c771a3ba13c2542147b95092670b79fd44031855` (linux/arm64). Restrict it to the projects
   that need it if you prefer.

What developers get: conversions of an upload folder with source or Dockerfiles, of Compose files with
`version:` 1, 2 or 3 (Move2Kube ignores Compose Specification files, so Samuori refuses them for it), and of Samuori images,
as manifests, a Helm chart or Kustomize overlays. Move2Kube answers its questions with defaults, often wrong for the
app (ports, Ingress for every port, registry), so the conversion waits for answers in the portal and runs again;
Samuori checks every answer. Not offered: CI/CD, Knative and OpenShift output, build scripts, existing Kubernetes YAML
or Helm as input, Cloud Foundry, buildpacks containerization and Windows ([spike](../spikes/move2kube.md)).

Status: the registry, policy and admission check are covered by `TestProvidersAndMethodPolicy`; Kompose
conversions by the tests listed under SEC-K8S-01..07, with real Kompose output as golden input; Move2Kube by
SEC-M2K-01..05, with recorded real Move2Kube runs as golden input. Both providers' conversion steps ran on the
enrolled worker of an all-in-one VM (`rootless-dedicated-vm`, arm64, 2026-10-24): Kompose on `examples/compose-shop`
with a published Samuori image substituted, Move2Kube on `examples/orders-api` through questions, answers and resume
(`../spikes/e2e-providers.md`). A conversion can substitute only a build or publication of its own project, so upload
the Compose file to the image's project.
