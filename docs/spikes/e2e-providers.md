# Practical run: the developer workflow through a controller and an enrolled worker

Date: 2026-10-24. Status: **Passed** for all seven steps on one all-in-one VM, after five fixes found by this run;
one open limitation (OD-29). First run of the Railpack, Paketo Buildpacks, Kompose and Move2Kube paths through the
real product (API → admission → dispatch → worker → upload → validation → publication) rather than through
`rootlesshost.Provider.Run` directly. Raw output is kept in the maintainer's private records.

## Setup

- One Lima VM from `deploy/dev/lima/samuori-clean.yml` (Ubuntu 26.04 arm64, 4 vCPU, 6 GiB, Apple Virtualization) on a
  macOS arm64 host, created fresh for this run and deleted afterwards. No other VM, no cloud resources.
- `make release` from the worktree, installed with `deploy/dev/lima/install-clean.sh dist/samuori-linux-arm64 samuori-clean
  --railpack --with-kompose` (later also `--with-move2kube`), then upgraded in place the same way after each fix. The
  final run used the branch rebased on main at 8c2aab2 (Move2Kube and the reworked conversion step).
- Native shape: strict egress (build-user firewall, egress proxy, docker.io mirror), `cgroup-limits=systemd`, the
  `local` worker of profile `rootless-dedicated-vm`.
- First-run setup, the developer `dana` (maintainer of team `payments`), enabling the `local` pool and its self-test,
  the local registry destination and a recipe build with publication ran through the browser with
  `make test-acceptance` (`vertical-slice.spec.ts`: 5 passed; `session-expiry.spec.ts` failed only because it ran
  before setup existed and was not re-run). Everything else used the API with session cookies and CSRF tokens, as
  the portal does.

## Administrator configuration (as `docs/operators/providers.md` describes)

| Item | Value |
|---|---|
| Pins checked against upstream before approving | `ghcr.io` and Docker Hub manifests by anonymous token: all five image digests in the guide matched; installed `railpack` `d70e585b…b042f`, `kompose` `24faf621…09894`, `move2kube` `2edf77ac…31855` matched the guide |
| Railpack | enabled `v0.43.0`, both binary sums, frontend, builder, runtime; presets `mise`, `nodejs-dist`, `npm`, `debian` |
| Paketo Buildpacks | enabled `v0.21.26`, builder `ubuntu-noble-builder:0.0.205@…793dd9`, run image `ubuntu-noble-run:0.0.150@…ab060`; presets `nodejs-dist`, `npm` |
| Kompose, Move2Kube | enabled `v1.38.0` and `v0.3.15` with both binary sums |
| Build method policy | `recipe`, `dockerfile`, `railpack`, `buildpacks`; default automatic |
| Base-image policy | added `ghcr.io/railwayapp/railpack-builder`, `ghcr.io/railwayapp/railpack-runtime`, `docker.io/paketobuildpacks/ubuntu-noble-builder`, `docker.io/paketobuildpacks/ubuntu-noble-run` |
| Validation policy | `requireSbom: true` for this run |
| Egress policy | `npm` added to `dockerfilePresets` (the default allows only OS package presets for Dockerfile builds) |

The worker reported `railpack v0.43.0`, `buildpacks 0.14`, `kompose v1.38.0` and, once installed, `move2kube v0.3.15`
to the controller; the providers page counted one online worker with the approved version for each.

## Results

| Step | Result | Evidence |
|---|---|---|
| 1. Upload | `examples/orders-api`, a Dockerfile variant of it and `examples/compose-shop` uploaded as archives through upload sessions | snapshot IDs below |
| 2. Discovery | orders-api: Node.js/Express, high confidence, `npm ci` / `npm run start`; the variant's Dockerfile analysed statically (digest-pinned base, port 3000); compose-shop: `api/Dockerfile` found, no language finding (no `package.json`) | — |
| 3. Method | `GET /projects/{id}/build-methods`: all four available | — |
| 4. Build `recipe` | build `01M5QJ1ZJ12T21CTP6RW4VYHBP` succeeded; validation passed (SBOM, sensitive content, `GET / answered 200`) | `recipe-*` |
| 4. Build `dockerfile` | build `01M5QHRXKM70X4S43RTEE6A890` succeeded; validation passed | `dockerfile-*` |
| 4. Build `railpack` | build `01M5QJHKZMJP5J2SR7MEKR4CMM` (final release) succeeded (22 s; 33 s for the first passing build); validation passed; `GET /builds/{id}/railpack` returns plan and info per attempt; egress `mise-versions.jdx.dev`, `nodejs.org`, `registry.npmjs.org`, `deb.debian.org`, all through the proxy; the frontend, builder and runtime images came through the new ghcr.io mirror | `final-railpack-*` |
| 4. Build `buildpacks` | build `01M5QJJJ7E28JEBF4VD6Z4Y2KQ` succeeded; validation passed with `BPE_DEFAULT_MEMORY_AVAILABLE=max` (see OD-29); `GET /builds/{id}/buildpacks` returns metadata and SBOM per attempt and `/buildpacks/sbom?attempt=1` the CNB SBOM archive (raw output kept in the maintainer's private records). The first buildpacks build's archive digest `04a67aae…f0521` was identical to the dev-VM spike's | `final-buildpacks-*` |
| 5. Publish | Railpack image published as `payments/orders-api:2.0.0-railpack` with signature and attestation; the registry's `Docker-Content-Digest` for the tag is `sha256:b6f33963…7c58`, the publication's `destinationDigest` | `publication-2.0.0-railpack.json`, `registry-check.txt` |
| 6. Kompose | compose-shop with `api` replaced by the published image: first Linux run of the conversion step, succeeded on the worker (tool digest `24faf621…`, `rootless-dedicated-vm`); re-run on the rebased step also succeeded | `conversion-rev1.json`, `conversion-shop-run2-rev4.json` |
| 6. Findings | errors `privileged` and `added-capabilities` (db); `host-volume-not-carried` (`./backups`); `secret-reference` for `DATABASE_PASSWORD` and `POSTGRES_PASSWORD`; `image-pinned` (api by digest); `probe-from-evidence` (HTTP readiness on 3000 from the build's runtime check) and `no-readiness-probe` for db and web, which have no evidence; `image-not-pinned`, `no-resources`, `may-run-as-root`, `exposed-outside-cluster` | as above |
| 6. Move2Kube | `examples/orders-api` as source input: `needs-answers` with ten questions; answered over three runs (port 3000, ClusterIP, the local registry, then the follow-up question for port 3000 and the service selection); run 4 succeeded on the worker with no network, and revision 4 holds a Deployment and a ClusterIP Service on port 3000 | `conversion-orders-m2k.json`, `conversion-m2k-answers3-rev4.json`, `orders-m2k-rev4.zip` |
| 7. Review and download | an edit removing `privileged` and `SYS_NICE` from db stored revision 2 (`origin: edit`, parent 1, errors 0, diff); an edit adding a literal `API_TOKEN` stored revision 3 with the value turned into a `secretKeyRef`; the revision 3 ZIP (raw output kept in the maintainer's private records) has 8 manifests and `samuori-review.json`, and neither the ZIP nor anything under `/var/lib/samuori` holds the example passwords or the canary token | `shop-rev3.zip` |

The conversion step's tool runner confines itself before it runs the tool and fails if any non-loopback interface
has an address, so every successful conversion above confirms the step ran with loopback only.

## Negative checks

| Check | Observed |
|---|---|
| Method not allowed (`railpack` removed from the policy) | admission `build-method-not-allowed`; the build is recorded as Blocked: "the railpack build method is not allowed here; …" |
| Provider disabled (buildpacks) | admission refuses; the build is Blocked: "Paketo Buildpacks (CNB lifecycle) is not enabled for this project and worker pool; …" |
| Conversion provider disabled (Kompose) | `422`: "Kompose is not enabled for this project; …" |
| Egress denial | the Dockerfile build before `npm` was allowed for Dockerfile builds failed at `npm ci`, and the log says `egress: denied registry.npmjs.org:443 (1000): not on the build's egress allowlist` (raw output kept in the maintainer's private records). It is classed as an application failure, without retries |
| Image from another project | a conversion naming a publication of another project: `422` "the publication … is not in this project" |
| Answers not asked | a re-run answering a question the last run did not ask: `422` naming it |
| Policies restored | method policy and provider configuration restored after each check |

## Bugs found and fixed

1. **Build context modes** (`fix(workeragent)`). The worker wrote source files 0600, so a Dockerfile that copies source
   and switches to a non-root `USER` built an image whose files that user could not read (runtime test:
   `EACCES … /app/server.js`). Source now gets 0644/0755 inside the 0700 attempt directory, as conversion input already
   did. `TestFetchSourceGivesTheBuildContextCheckoutModes`.
2. **Railpack planning ignored the egress proxy** (`fix(railpack)`). Railpack v0.43.0 runs mise with only `PATH` set
   (its `runCmdWithEnv`), so under the worker firewall `mise latest node@22` failed on DNS
   (raw output kept in the maintainer's private records); the dev-VM spike passed because no firewall was
   applied. Planning also fails fully offline (checked with `unshare -n`), so a proxy route is required.
   `install.sh --railpack` now installs mise 2026.10.14 (the version Railpack v0.43.0 plans with; musl archive checked
   against mise's `SHASUMS256.txt`); the worker checks its SHA-256 against the pin for the approved Railpack version
   and stages it where Railpack looks, behind a wrapper that adds the build's proxy. Without a staged mise,
   strict-egress planning fails with a clear reason. This also removes Railpack's unverified mise download.
   `TestRailpackMiseStaging`.
3. **Proxy credential in the journal** (`fix(railpack)`). Planning received the egress-proxy URL, with its per-build
   credential, on the `sudo` command line, which sudo logs (2 journal lines before the fix, 0 after). It is now read
   from a 0640 file in the attempt directory. `TestRailpackPlanningKeepsTheProxyOffTheCommandLine`.
4. **No ghcr.io mirror** (`fix(railpack)`). Under strict egress the daemon pulls only through mirrors, and the host
   mirrored only docker.io, so the frontend could not be resolved
   (raw output kept in the maintainer's private records). `--railpack` now runs `samuori-mirror-ghcr`
   (ghcr.io pull-through on `10.231.0.1:5002`), passes it to the worker's `--mirrors` and allows it in the firewall.
5. **Compose probe kept without a finding** (`fix(kubeconvert)`). Kompose turned the api service's healthcheck into
   an exec liveness probe (`wget -qO- http://localhost:3000/healthz`) written for the image the Samuori image replaced;
   the Samuori image has no `/healthz`. It is kept, as policy requires, and now reported as `probe-from-replaced-image`.
   `TestDigestSubstitutionAndProbes`.

## Open limitation: runtime tests and cgroup files (OD-29)

The runtime test runs inside a rootless BuildKit `RUN` step, which has no `/sys` at all and no `CAP_SYS_ADMIN`
(raw output kept in the maintainer's private records). Paketo's node-engine runs an `exec.d` helper at
launch that reads `/sys/fs/cgroup/memory.max` (or the v1 file) and exits when neither exists, so every default Paketo
Node.js image fails Samuori's runtime test with `exited with status 82`
(raw output kept in the maintainer's private records), although it starts under Docker or Kubernetes. The
probe cannot mount cgroupfs there, and synthesizing cgroup files would test a fake environment, so this is left open
with a recommendation. For this run, the developer-level setting `BPE_DEFAULT_MEMORY_AVAILABLE=max` sets the launch
variable that makes the helper skip the lookup; it does not change behaviour unless `OPTIMIZE_MEMORY` is set.
Update 2026-10-26: the owner decided OD-29 (a read-only view of the test's own cgroup); a source review found that a
rootless BuildKit step cannot get one without `--security=insecure` ([spike](runtime-test-cgroup-view.md)), so the
setting stays.

## Observations

- Uploading Compose files to the image's own project is required for image substitution; `sourceDir: "."` is
  refused for a Move2Kube source input (leave it empty for the upload root).
- Move2Kube returns to `needs-answers` until every question it asked is answered, including the service selection;
  question IDs change with answers (`"8080"` becomes `"3000"`).
- Builds blocked by an upgrade's self-test (`worker-config-changed`) stay Blocked; a new build is needed once the
  worker passes again (20–50 s here).
- `/tmp/railpack` is created on the host's shared `/tmp`, owned by `samuori-build`, empty (each attempt bind-mounts its
  own directory over it).
- A disabled provider's admission error reuses the code `build-method-not-allowed`; the build's blocked reason names
  the provider.

## Not covered

- amd64, `vm-per-job` (no providers in the guest), remote workers, PostgreSQL, Compose install shape.
- The portal for steps 2–7 (only the acceptance journey ran in a browser); `session-expiry.spec.ts` was not re-run.
- A buildpacks or Railpack build with a bound build secret through the controller (the spikes covered secrets on the
  worker path only), and a lifecycle or mise cache.
