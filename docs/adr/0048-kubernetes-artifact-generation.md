# 0048. Kubernetes artifact generation: Kompose and Move2Kube

Status: Accepted (2026-10-24) — owner directive "Unified Build and Kubernetes Conversion Integration". Amends the
scope in `../product/vision-and-scope.md`: Samuori generates and validates deployment artifacts; it still never deploys,
reconciles or provisions clusters. Implementation: Kompose conversions built (2026-10-24,
`../architecture/kubernetes-conversion.md`), not yet run on a Linux worker; Move2Kube v0.3.15 built (2026-10-24,
`../architecture/kubernetes-conversion.md#move2kube`, `../spikes/move2kube.md`): offline, non-interactive runs with
review → answer → resume, golden tests from real runs on macOS, not yet run on a Linux worker; it supports source
folders, Dockerfiles, versioned Compose files and Samuori images, not Compose Specification files or existing
Kubernetes YAML as input; `vm-per-job` execution not built.

## Context
After building an image, teams need Kubernetes manifests. Kompose converts Docker Compose files; Move2Kube analyses
source, Dockerfiles and Compose files and emits manifests, Helm charts or Kustomize overlays. Both are Apache-2.0.
Move2Kube's last release (v0.3.15) was in March 2025, built from its last commit of November 2024 (checked 2026-10-24).

## Decision
- **Conversion is a job of its own**, separate from builds: a `Conversion` resource in a project with an input
  (snapshot path, Compose file, or a built or published image), a provider, options (format, name, namespace, ports,
  exposure, replicas, resources, storage) and versioned results. It uses the existing operations, authorization,
  audit, worker dispatch and artifact storage; no new scheduler, store or identity.
- **Providers behind one interface**, `kompose` and `move2kube`, each pinned to an approved version and run on a
  worker inside the build isolation with no network and no credentials, over a copy of the untrusted input, with
  time and resource limits. Output is untrusted until Samuori validates it.
- **Questions:** Move2Kube runs non-interactively; the questions it would ask, with defaults, are returned to the job,
  shown in the portal, and the job re-runs with the answers (`review → answer → resume`).
- **Samuori post-processing, never invention:** parse every document, check it against the schemas of the targeted
  Kubernetes version, substitute the published image digest when the input is a Samuori image, turn literal secret values
  into Secret references with a finding, and report privileged containers, host namespaces, `hostPath`, added
  capabilities and similar against Samuori policy. Probes are added only from evidence (the image's validation profile
  and port); everything else that needs operator judgement is marked for review.
- **Optional and administered:** both providers are disabled until an administrator approves a version; Move2Kube
  stays disabled by default and its page states its maintenance status.
- **Results** are versioned artifacts with lineage (snapshot, BuildSpec, image digest, provider and version, options,
  findings), previewed and edited in the portal, and downloaded as a ZIP without credentials.

## Consequences
- Generated artifacts are a starting point that needs operator review; Samuori states what it could not translate.
- Image-only inputs yield only what the image and Samuori's records show; configuration absent from both is reported
  missing, not guessed.
- Move2Kube may need replacing if it stays unmaintained.
