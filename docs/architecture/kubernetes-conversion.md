# Kubernetes conversion

Generating Kubernetes deployment artifacts from a project's Compose file, source or a Samuori image (ADR 0048). Samuori generates and
validates; it never applies, reconciles or provisions anything in a cluster (`../product/vision-and-scope.md`).
Code: `internal/kubeconvert` (domain, store, dispatch), `internal/convertjob` (controller–worker job contract),
`internal/toolrun` (the in-step tool runner), `internal/workeragent/convert.go`,
`internal/adapters/isolation/rootlesshost/convert.go`. User guide: `internal/help/content/kubernetes.md`; operator
guide: `../operators/providers.md`.

## Status

| Part | State |
|---|---|
| Conversion resource, runs, revisions, findings, lineage (migration 00055) | implemented; focused tests on SQLite; migration 00055 applied, rolled back and re-applied on PostgreSQL 16 (2026-10-24, scratch database) |
| Kompose provider (plan and interpretation) | implemented; golden tests consume a real Kompose v1.38.0 run (see [Evidence](#evidence)) |
| Post-processing and validation against Kubernetes 1.36 | implemented; focused tests |
| Worker dispatch, leases and the conversion job kind (protocol 1.4) | implemented; focused tests with a fake channel and a fake isolation provider |
| Execution in `rootless-dedicated-vm` (and `rootless-host` for trusted pools) | implemented; ran on the enrolled worker of an all-in-one VM (arm64, 2026-10-24) for Kompose and Move2Kube ([practical run](../spikes/e2e-providers.md)) |
| Execution in `vm-per-job` | **not implemented**: the KVM provider does not implement `isolation.Converter`, so its workers do not advertise providers and receive no conversion |
| API and UI (wizard, conversion page, history) | implemented; API tests; UI type-checked, linted and built, not yet checked in a browser |
| Move2Kube v0.3.15 (plan, interpretation, questions, answers, re-run) | implemented, **off by default** (unmaintained since March 2025); golden tests consume real runs on macOS with the network denied ([spike](../spikes/move2kube.md)); ran on a Linux worker through questions, answers and resume ([practical run](../spikes/e2e-providers.md)) |
| Wizard provider list and questions on the conversion page | implemented; type-checked, linted and built, not yet checked in a browser |

## Resource and lifecycle

A **Conversion** belongs to a project: a name (DNS label), a provider, an input, options and a state. Each run of the
provider is a **ConversionRun** with its own lease, fencing generation and attempts, separate from BuildRun,
ValidationRun and Publication. A successful run stores **revision n**; an edit stores the next revision with the
edited revision as its parent. Ready means a revision exists, never that anything was applied.

```
queued → assigned → running → succeeded | needs-answers | failed | timed-out
```

Only one run of a conversion is active at a time. Creating and re-running use the operations table and an
`Idempotency-Key`; create, re-run, edit and download are audited (`kube-conversion.*`).

**Inputs:** `compose` (an upload and the Compose file's path, optionally a Samuori build or publication whose image
replaces one service's image), `build` or `publication` (image only: Samuori writes a one-service Compose file from its
records and says so in a finding; environment, volumes and other services are reported missing, not guessed),
`source` (Move2Kube only: an upload folder with source or Dockerfiles, optionally with a Samuori build or publication
for one service). **Options:** format (`manifests`, `helm`, `kustomize`), namespace, replicas,
exposure of one service (cluster IP, node port, load balancer, Ingress host and path), default requests and limits,
storage (persistent volume claims or empty directories, size, storage class; `hostPath` is refused), and the
targeted Kubernetes version (1.36).

**Lineage** on every revision: project, input, snapshot digest, BuildSpec and revision, build, image digest and
reference, provider, approved version and the SHA-256 of the tool that ran, options, Kubernetes version, run, worker
and isolation profile. It never holds credentials.

## Providers

`kubeconvert.Provider` (domain package, no vendor SDK) has `Plan` (the argv after the tool path, output
directories) and `Interpret` (bundle files, the tool's warnings as findings, and **questions** with defaults). It is
registered by the name used in the provider catalog (`internal/providers`). A plan may also carry small
configuration files for the tool (written read-only to `/work/files`) and a keep list (only those output paths
leave the step). Providers that ask questions validate answers (`Asker`); providers that read Compose files
differently may refuse one before a run is queued (`ComposeChecker`).

**Kompose** runs `kompose convert -f /in/<compose> -o /out/<dir> --provider kubernetes --build none
--push-image=false --with-kompose-annotation=false --replicas N --volumes <kind>` plus `--namespace`,
`--pvc-request-size` and `--chart` for Helm. `--build none` means it never builds or pushes; without annotations no
input path reaches the output. Every `WARN` line becomes a finding; those that drop or change configuration (a host
bind mount that becomes a claim without its data, a service without ports, unsupported options) need review.
Kompose writes no Kustomization; for the `kustomize` format Samuori lists the manifests in `kustomization.yml` and says
so.

## Move2Kube

Move2Kube runs once per conversion run, non-interactively, as

```
move2kube transform --source /in[/<folder>] --output /out/m2k --name <name> --qa-skip
  --qa-cache-out /out/m2k/.m2k-qa --config-out /out/m2k/.m2k-qa --disable-local-execution --ignore-env
  --config /work/files/samuori-move2kube.yml
```

`transform` without a plan file plans and transforms in the same run, so one tool run is enough; `--qa-skip` answers
every question with its default or the configuration file and never prompts. The configuration file holds the
user's validated answers, then Samuori's fixed settings, which always win: `spawncontainers: false`, `minreplicas` from
the replicas option, the `Kubernetes` cluster type, and `transformers.types` limited to analysers (Compose,
Dockerfile, language and Java), `Kubernetes`, `ClusterSelector` and `Parameterizer`. Build and push scripts, CI/CD,
Knative, operators, Cloud Foundry, existing Kubernetes YAML, Windows and Cloud Native Buildpacks (which needs
containers) are not offered. The keep list is the QA cache and the chosen format's folder: `deploy/yamls`
(manifests), `deploy/yamls-parameterized/helm-chart/<name>` (Helm) or `deploy/yamls-parameterized/kustomize`
(Kustomize, a base and dev, staging and prod overlays); Move2Kube's copy of the input and everything else is removed
inside the step.

**Questions:** Samuori reads Move2Kube's QA cache (`m2kqacache.yaml`): id, kind (`Select`, `MultiSelect`, `Confirm`,
`Input`, `MultiLineInput`), description, hints, options, default and the value used. Questions Samuori answers itself
are not offered: transformer selection, containers, replicas, cluster type, registry logins (credentials), CI/CD and
passwords. A run with any offered question unanswered ends in `needs-answers` with a revision generated from the
defaults; the portal shows the questions; a re-run sends the answers. Samuori validates them against the questions of
the last run that asked any: only those ids, options from the list (a typed value only where Move2Kube offers
`Other`), typed inputs (ports, URL paths, DNS names, registry hosts and paths, plain text without template, quote or
shell characters), and never `HostPath` storage. A list answer is a JSON array, a `Confirm` is `true` or `false`.
The configuration key of an answer is its id split at dots outside quotes; `services.[].enable` becomes one
`enable` per service. A re-run with options only keeps the previous answers that the last run still asked. Answers
are stored with the run and never hold credentials.

**Inputs Move2Kube cannot read:** it silently ignores Compose Specification files (no `version:`, or a top-level
`name:`), so Samuori refuses them for Move2Kube before a run and points to Kompose. It does not carry service settings
such as `privileged: true` reliably (dropped in 3 of 6 identical runs, [spike](../spikes/move2kube.md)), so Samuori
reads the Compose file before the run and adds a review finding for each service that sets privileged, cap_add,
security_opt, devices or a host network, pid or ipc namespace. When it generates its own Dockerfile for a service
(its language transformers win over an upload's Dockerfile), a review finding says so: the image and port in the
manifests then come from that Dockerfile, which Samuori does not keep.

**Interpretation:** `level=warning` lines become findings (an ignored volume is `storage-not-carried`, review);
two `error` lines Move2Kube always logs in the sandbox about its input copy and build scripts become one info
finding; other errors are warnings; `fatal` fails the run. No file for the chosen format fails the run with the reason.
Its output then goes through the same post-processing as Kompose's.

## Execution

Conversions use the worker channel, leases and isolation that builds use; there is no second scheduler.

- **Dispatch** (`kubeconvert.Scheduler`) assigns a queued run only to a worker that: reports protocol 1.4 or later;
  reports the provider at the **approved version** (`Capabilities.providers`); is in a pool and project the
  administrator's provider approval allows; has an approved checksum for its architecture; and passes the same
  eligibility as an **untrusted** build (approved untrusted-capable profile, fresh passing self-test for its current
  configuration, strict egress, not cordoned). Builds and conversions share daemon slots
  (`workers.BusySlotsQuery`, OD-16). The assignment carries identifiers and the job digest only.
- **Leases** are fenced like build attempts: every call names the attempt and generation; a stale generation, another
  worker or a finished run is refused. Conversion leases are renewed in their own `Heartbeat` field, so the build
  scheduler never cancels them; lapsed leases are requeued (three attempts) or failed.
- **On the worker** (`workeragent.convert`): the job must match the digest bound to the lease; the tool
  `<providers-dir>/<name>/<name>` with `VERSION` equal to the approved version is copied into the attempt directory
  while hashed, and runs only if the copy's SHA-256 equals the approved one (`policy` failure otherwise, nothing
  prepared); the input must match the job's input digest file by file.
- **In isolation** (`isolation.Converter`, `Manager.Convert`): a fresh environment per run, with no secret
  release, no mirror token and no cache. `rootlesshost` runs one BuildKit solve of a generated Dockerfile:

  ```
  FROM scratch AS samuori-convert
  WORKDIR /work
  RUN --network=none --mount=type=bind,from=samuoritool,target=/.samuori-tool --mount=type=bind,from=samuoriinput,target=/in ["/.samuori-tool/samuori-run","tool-run","@/.samuori-tool/spec.json"]
  FROM scratch
  COPY --from=samuori-convert /out/ /
  ```

  Nothing is pulled; the tool, the run specification (`spec.json`) and the input are read-only local contexts; the solve sets no proxy or build arguments and
  its secret provider refuses every request; output is a local export. `samuori-run` is the worker's own static binary
  (`samuori-worker tool-run`): as the step's first process it applies the runtime probe's confinement (no_new_privs,
  empty capability bounding set, loopback-only network), writes the job's configuration files to `/work/files`
  (root-owned, read-only), then runs the tool as uid 65534 in `/work/run` with only `HOME` and `TMPDIR`
  (`/work/tmp`, beside the working directory: Move2Kube's path handling breaks when its temporary directory is below
  it) and an unusable `PATH`, kills it at the provider's time limit, removes every output path not on the keep list
  (without following symbolic links), and always writes `/out/.samuori/result.json`, stdout and stderr (each capped at
  256 KiB). Memory and processes are bounded by the daemon's systemd scope.
- **Why a BuildKit step rather than running the binary directly:** the rootless profiles have no sandbox for an
  arbitrary host process; the build step is the boundary Samuori already verifies by self-test, gives `--network=none`
  without new firewall rules, and starting `FROM scratch` needs no base image, so nothing is pulled or trusted
  besides the approved tool. The KVM profile would need the same contract over its guest channel (not done).
- **Report:** at most 500 regular files, 1 MiB each, 3 MiB in all, below the planned output directory; symbolic
  links fail the run. The controller refuses a report whose tool digest is not the approved one.

## Validation

Post-processing on the controller (`kubeconvert.Process`) parses only and never executes:

1. Every `.yaml`, `.yml` and `.json` document is parsed (templated Helm files are reported, not validated).
2. User options and Samuori records are applied, each change reported: the Samuori image is pinned by digest in its
   service's containers (an unpublished image is an error to fix); a readiness probe is added **only** when the
   build's runtime check names a port (HTTP for `http-startup`/`static-serve`, TCP otherwise), else the missing
   probe is a review finding; an exec probe Kompose made from the replaced service's Compose healthcheck is kept and
   reported for review (`probe-from-replaced-image`), since it was written for the other image; exposure, default
   resources and the storage class.
3. **Secrets:** an environment value that looks like a credential (by name, or a URL with a password) is replaced by
   `valueFrom.secretKeyRef` to `<workload>-secrets`; Secret objects lose their values; ConfigMap values that look like
   credentials are removed. Each is a finding; the value is never stored in a revision or a ZIP.
4. **Policy** (errors): privileged containers, `hostNetwork`/`hostPID`/`hostIPC`, `hostPath`, added capabilities,
   `allowPrivilegeEscalation`, `runAsUser: 0`, unmasked proc or unconfined seccomp. Review: possible root user,
   images not pinned by digest, missing resources or readiness probes; warnings: host ports, node-port and
   load-balancer services. These settings are kept and reported, never dropped.
5. Kustomize: kustomization files are not objects and are not validated; files they name as patches are
   reported as patches, not objects. Samuori writes a `kustomization.yml` only when the provider wrote none.
6. **Schemas:** every object is validated against the schemas of Kubernetes v1.36.5 extracted by `tools/kubeschema`
   from that release's OpenAPI document (`internal/kubeconvert/schemas/kubernetes-v1.36.json`, 99 KB, 18 kinds and
   the definitions they reference). The extraction rejects unknown fields where a schema lists its properties, as
   strict server-side validation does, and accepts integers for int-or-string and numbers for quantities. Other
   kinds are reported as not validated. Regenerate: `cd tools/kubeschema && go run . -swagger swagger.json -version
   v1.36.5 -out ../../internal/kubeconvert/schemas/kubernetes-v1.36.json`.
7. Manifests get the `.yml` extension; Helm's `Chart.yaml` and values files (`values.yaml`, `values-<env>.yaml`)
   keep the names Helm requires, and a folder with a provider's `kustomization.yaml` keeps the names it refers to.
8. The namespace option is set on every namespaced object that names none (Move2Kube has no namespace option);
   templated Helm files are not changed.

**Edits** store a new revision and run steps 1 and 3–7 again over every file; findings from the provider and from
options are carried over.

## API

`/projects/{projectId}/kube-providers` (the conversion providers the project may use, their inputs, formats and
maintenance warning; no pins or scope), `/projects/{projectId}/kube-conversions` (list, create), `/kube-conversions/{id}` (get), `…/runs` (list, re-run),
`…/revisions`, `…/revisions/{n}` (files, findings, lineage), `…/revisions/{n}/findings`, `…/revisions/{n}/file?path=`
(get, edit), `…/revisions/{n}/bundle` (ZIP with `samuori-review.json`). Read: any role on the project; create, re-run and
edit: developer, maintainer or administrator. A re-run takes options and, for Move2Kube, `answers` (id → value);
omitted answers keep the previous ones. Another team gets 404 on every call (SEC-K8S-06).

## Evidence

Real Kompose, recorded 2026-10-24 on macOS arm64: the official `kompose-darwin-arm64` v1.38.0
(`kompose version`: `1.38.0 (a8f5d1cbd)`), SHA-256 `5687d79633854d5fc8151a0148fd6045b643e3f745f7413106c807fe63fe97eb`,
matching the release's `SHA256_SUM`, run on `examples/compose-shop` (web, api, postgres with ports, environment,
volumes, a bind mount, `depends_on`, a health check, resource limits, `cap_add` and `privileged`):
`kompose convert -f in/compose.yml -o out --build none --with-kompose-annotation=false --namespace shop` (exit 0,
8 files, four `WARN` lines) and the same with `--chart`. The outputs are the golden files in
`internal/kubeconvert/testdata/`. `TestRealKompose` (`internal/toolrun`) runs a binary named by `SAMUORI_KOMPOSE_BINARY`
through the tool runner with an empty environment. Neither run exercised Linux isolation; the conversion step on a
Linux worker is recorded in [`../spikes/e2e-providers.md`](../spikes/e2e-providers.md).

Real Move2Kube, recorded 2026-10-24 on macOS arm64 with the network denied (`sandbox-exec`, `deny network*`): the
official `move2kube-v0.3.15-darwin-arm64.tar.gz` (SHA-256 matching the release's `.sha256sum`), binary
`sha256:021fe01b…f43f4a`, run with Samuori's argv, configuration and keep list by `TestRealMove2Kube`
(`internal/toolrun`, `SAMUORI_MOVE2KUBE_BINARY`) on `examples/compose-shop` (with `version: "3.8"` instead of
`name: shop`) through defaults, answers and resume, and on `examples/orders-api` with and without a Dockerfile.
The recordings are `internal/kubeconvert/testdata/move2kube-v0.3.15-*.json`; the commands, findings and what is not
supported are in [`../spikes/move2kube.md`](../spikes/move2kube.md). No run exercised Linux isolation.
