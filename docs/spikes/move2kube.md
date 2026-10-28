# Spike: Move2Kube offline and non-interactive

Date: 2026-10-24. Status: **Passed on macOS arm64 with the network denied; not yet run in a Linux conversion step.**
Implements the Move2Kube half of ADR 0048 (`../architecture/kubernetes-conversion.md#move2kube`). No VM was started:
the in-worker Linux run (`FROM scratch`, `--network=none`, uid 65534) is still to be done.

## Release

Move2Kube v0.3.15 (Apache-2.0, GitHub license API), released 2025-03-07 from commit `7012d98` of 2024-11-06, which is still the head of `main` and `release-0.3`; no release or commit since (checked 2026-10-24). Assets exist
for darwin amd64/arm64, linux amd64/arm64/ppc64le/s390x and windows amd64, each as `.tar.gz` and `.zip` with a
`.sha256sum` file. The archives hold `move2kube/move2kube`, `LICENSE`, `USAGE.md` and `samples/`.

| Asset | Published and computed SHA-256 (equal) | `move2kube` binary SHA-256 |
|---|---|---|
| `move2kube-v0.3.15-darwin-arm64.tar.gz` | `2d4ecd99a91269bf73097d48564edeb6a943d5f9a8874a8ab19b91258e396264` | `021fe01b4170c5a0692827d1e1dee7d95b66abdcece6e753ca309a3b42b43f4a` (Mach-O arm64) |
| `move2kube-v0.3.15-linux-amd64.tar.gz` | `5bcda9f36be300a5b161b56e93bb4ca93920fa6e16ebaf34453f0778dddb5c0f` | `7f63f3ab9cd04ba9d887a59ff68d409612da26274e7c44d3efb6dbaec0afb501` (ELF x86-64, statically linked) |
| `move2kube-v0.3.15-linux-arm64.tar.gz` | `d2a28dea9886c6d3110870be321fe69f1f5d9335b9031504ca4003e5265977bd` | `2edf77acd01ff02e3e54fa72c771a3ba13c2542147b95092670b79fd44031855` (ELF aarch64, statically linked) |

`move2kube version -l`: `v0.3.15+unreleased`, git commit `7012d9892fc3c4dcfac3744f42ff2ea538a471bb`, tree clean,
go1.19.13. The Linux binaries are static, so they run in a `FROM scratch` step without a base image.

## Method

- No network: every run went through `/usr/bin/sandbox-exec` with the profile `(version 1) (allow default) (deny
  network*)`; under it `curl https://github.com` fails with `Could not resolve host` (exit 6).
- Empty environment: `HOME` and `TMPDIR` only (and `PATH=/nonexistent`), as the tool runner sets them.
- Exploratory runs used shell scripts in a scratch directory; the recorded runs used
  `TestRealMove2Kube` (`internal/toolrun/move2kube_real_test.go`), which plans each run with
  `kubeconvert.Move2Kube.Plan`, moves the sandbox paths (`/in`, `/out`, `/work`) below a temporary directory, runs it
  through the runner's `execute` (configuration file, working directory, prune) and interprets the kept output:

  ```
  go test -c -o toolrun.test ./internal/toolrun
  cd internal/toolrun && SAMUORI_MOVE2KUBE_BINARY=<darwin-arm64 move2kube> SAMUORI_MOVE2KUBE_RECORD=1 \
    sandbox-exec -f nonet.sb ../../toolrun.test -test.run TestRealMove2Kube -test.v
  ```

  The recordings are `internal/kubeconvert/testdata/move2kube-v0.3.15-*.json` (command, binary SHA-256, configuration,
  stderr with the sandbox paths restored, kept files).

## Findings

| # | Observation | Consequence in Samuori |
|---|---|---|
| 1 | `move2kube transform --source <dir>` without `--plan` plans and transforms in one run. With `--plan <file>` naming a file that does not exist it fails (`Error while accessing plan file`). | One tool run per conversion run; no plan file is passed. A separate `plan` run is not needed: planning questions (which services) are also asked by `transform`. |
| 2 | `--qa-skip` answers every question with its default and never prompts; the QA cache (`--qa-cache-out`) lists every question asked with id, type, description, hints, options, default and answer; `--config` files answer questions by their id path. A custom value for a `Select` with `Other (specify custom option)` (port `3000`) is accepted from the configuration. | Questions come from the cache; answers go back as a configuration file. |
| 3 | `--qa-cache-out` and `--config-out` naming a path that does not exist write a *file* there (both to the same file); naming a directory writes `m2kqacache.yaml` and `m2kconfig.yaml` inside it. | The plan creates `/out/m2k/.m2k-qa` first. |
| 4 | With `TMPDIR` at or below the working directory, path handling breaks: a literal `{{ .OutputPath9727887 }}` folder replaces `deploy/yamls`, parameterized output is missing, 8 to 9 `error` lines. Matrix: cwd `work`, `TMPDIR=work/tmp` fails; cwd `work/run`, `TMPDIR=work/tmp` and cwd `work`, `TMPDIR` elsewhere work. | The runner runs every tool in `/work/run` with `HOME` and `TMPDIR` at `/work/tmp` beside it. |
| 5 | Two `error` lines remain in every working run, about the `Source` path mapping (`path /in points to a unknown path`) and the Dockerfile build script (`relative to the env output dir ''`); the generated manifests are unaffected. | Reported as one info finding; the source copy and scripts are not kept. |
| 6 | Move2Kube ignores the Compose Specification: `examples/compose-shop/compose.yml` (no `version:`, top-level `name:`) gives `unsupported Compose file version: 1.0` (debug log) and only the `api/Dockerfile` service is converted, silently. `version: "3.8"` with `name:` is ignored too. `version: "3.8"` without `name:` converts all three services. | Samuori refuses such Compose files for Move2Kube before queueing a run and says to use Kompose; the wizard says so. |
| 7 | Output is not deterministic: six identical runs gave different file digests (environment order), and `privileged: true` of the `db` service became `privileged: false` in 3 of 6 runs; `cap_add: [SYS_NICE]` was kept in all six. | Samuori reads the Compose file before the run and adds a review finding for each service setting Move2Kube does not carry reliably (privileged, cap_add, security_opt, devices, host network, pid or ipc); policy findings still catch what is in the output. |
| 8 | Defaults are often wrong for the app: a port Move2Kube cannot find defaults to `8080` (Postgres `db`), every port defaults to an Ingress, the registry to `quay.io/<name>`, replicas to 2. | Questions are shown with the default used; the conversion stays in `needs-answers` until every one is answered; replicas come from Samuori's option. |
| 9 | For `examples/orders-api` with a Dockerfile added (`EXPOSE 3000`), Move2Kube chose its `Nodejs-Dockerfile` generator (`Found multiple transformation options ... Selecting the first valid option`), so the manifests use its generated Dockerfile's port 8080, not the upload's. Without a Dockerfile the same generator is used. | A review finding names the service and generator; Samuori does not keep the generated Dockerfile. |
| 10 | Every run copies the input into `<out>/<name>/source/` and writes build and push scripts, Tekton, Argo CD, Knative, BuildConfig, OpenShift templates and a README unless their transformers are deselected. With the input made read-only (`chmod -R a-w`) the copy fails with permission errors (89 `error` lines, no manifests): it copies the input's modes, so directories without owner write cannot be filled. | The configuration selects only analysers and the Kubernetes and Parameterizer transformers; the job's keep list removes everything except the chosen format and the QA cache inside the step. In the step the input is 0755/0644 (owner write), so the copy should succeed; **not yet verified on Linux**. |
| 11 | A transformer definition in the uploaded source (a Starlark transformer writing a marker file, and an `Executable` transformer running `touch`) was not loaded, with and without Samuori's transformer list; neither marker appeared. Customizations load only from `--customizations` (default `m2k-default-customizations` in the working directory, empty in Samuori). | Recorded; `--disable-local-execution`, no container runtime and `spawncontainers: false` remain the controls. |
| 12 | No run attempted a container runtime: `Couldn't find Podman ... Using Docker` appears only when the build-script transformers run, which Samuori deselects. `CNBContainerizer` needs containers and is deselected. | Recorded. |

## Recorded runs (Samuori's argv, no network)

Command (paths are the sandbox's):

```
move2kube transform --source /in --output /out/m2k --name shop --qa-skip --qa-cache-out /out/m2k/.m2k-qa
  --config-out /out/m2k/.m2k-qa --disable-local-execution --ignore-env --config /work/files/samuori-move2kube.yml
```

| Recording | Input | Exit | Kept files | Questions offered |
|---|---|---|---|---|
| `shop-defaults` | `examples/compose-shop` with `name: shop` replaced by `version: "3.8"`, manifests | 0 | 7 manifests + QA cache | 17 (none answered) |
| `shop-answered` | same, 10 answers (db port 5432, service types, storage PVC, ingress host, registry) | 0 | 9 manifests (2 claims) + QA cache | 16; 7 unanswered, including the new `"db"."5432"` service type and path |
| `shop-resumed` | same, every question of run 2 answered | 0 | 9 manifests + QA cache | 16, all answered: `succeeded` |
| `orders-source-helm` | `examples/orders-api` (no Dockerfile), Helm | 0 | `Chart.yaml`, 3 templates, `values-{dev,prod,staging}.yaml` + QA cache | 10 |
| `orders-dockerfile-kustomize` | `examples/orders-api` with a Dockerfile, Kustomize | 0 | base (3 objects and `kustomization.yaml`), dev/prod/staging overlays with patches + QA cache | 10 |

`TestRealMove2Kube` output (2026-10-24):

```
=== RUN   TestRealMove2Kube/compose_with_answers
    move2kube_real_test.go:149: run 1: 7 files, 17 questions, 8 findings
    move2kube_real_test.go:178: run 2: 16 questions, new: [move2kube.services.web.deployment move2kube.services.db.deployment move2kube.services.api.deployment move2kube.services."db"."5432".servicetype move2kube.services."db"."5432".urlpath move2kube.target."default".ingress.ingressclassname move2kube.target."default".ingress.tls]
=== RUN   TestRealMove2Kube/source
    move2kube_real_test.go:202: source: 7 files, 10 questions
=== RUN   TestRealMove2Kube/source_with_Dockerfile
    move2kube_real_test.go:214: dockerfile: 10 files, 10 questions
--- PASS: TestRealMove2Kube (0.86s)
```

("new" lists the questions of run 2 the answers did not cover.) In run 3 the `db` Service and container use port
5432 and two persistent volume claims replace the ignored volumes.

## Supported and not supported

Supported (offline, non-interactive): an upload folder with Dockerfiles or source Move2Kube recognises (Node.js,
Python, Go, Java with Maven, Gradle, WAR or EAR, PHP, Ruby, Rust, .NET); Compose files with version 1, 2 or 3 and no
other top-level keys; Samuori images (a one-service Compose file from Samuori's records). Outputs: plain manifests
(`deploy/yamls`), the Helm chart and the Kustomize base with overlays (`deploy/yamls-parameterized`).

Not supported: Compose Specification files (use Kompose); existing Kubernetes YAML, Helm or Kustomize as input
(`KubernetesVersionChanger` is deselected); Cloud Foundry manifests; Cloud Native Buildpacks containerization (needs a
container runtime); Windows containers; Tekton, Argo CD, BuildConfig, Knative, operator and OpenShift template output;
build and push scripts. Only verified on the two examples above.

## Not yet verified

- The run inside a Linux worker's conversion step (`rootless-dedicated-vm` or `rootless-host`): the linux binaries,
  uid 65534, `--network=none`, the read-only input mount and the keep list in a BuildKit step.
- `vm-per-job` (conversions are not implemented there).
- Larger applications against the output limits (500 files, 3 MiB) and the 256 KiB log cap; the source copy is
  removed in the step, so only Move2Kube's chosen output counts.
- The portal pages in a browser.
