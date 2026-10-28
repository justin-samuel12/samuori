# Spike: Image Labels and Annotations through BuildKit

Date: 2026-10-26. Status: **Complete; consistent with `build-execution.md#image-labels-and-annotations`.**

## Source check (BuildKit v0.34.0, the version in `go.mod`)

- `frontend/dockerui/config.go`: frontend options with the prefix `label:` become `Config.Labels`.
  `frontend/dockerfile/dockerfile2llb/convert.go` copies them into the target stage's image config after the
  Dockerfile's own `LABEL` instructions.
- `exporter/containerimage/exptypes/annotations.go`: exporter attributes match
  `^annotation(?:-([a-z-]+))?(?:\[([A-Za-z0-9_/-]+)\])?\.(\S+)$`. A bare `annotation.<key>` is a manifest annotation
  for every platform; `annotation-index.<key>` an index annotation; a `[platform]` qualifier limits a manifest
  annotation to one platform.
- `exporter/containerimage/writer.go`: index annotations on a single-platform export fail with "index annotations not
  supported for single platform export".
- `exporter/oci/export.go`: the OCI exporter merges the attributes with frontend metadata and sets
  `org.opencontainers.image.created` on the archive's `index.json` descriptor (export time, or the epoch) when it is
  absent.

## Practical run

A disposable `moby/buildkit:v0.34.0` container (privileged, TCP on loopback) on the macOS development host. A
temporary in-package test called `Daemon.Build` with a `Request` whose `Labels` and `Annotations` were set, so the
solve used exactly `Attrs` and `ExportAttrs`. Dockerfile: `FROM scratch`, `LABEL com.example.team=from-dockerfile
com.example.kept=yes`, `COPY a /a`. The archives were read back with `internal/imagescan`. The container, its image
and the temporary test were removed afterwards.

| Platforms | Config labels | Manifest annotations | Index annotations |
|---|---|---|---|
| `linux/amd64` | `com.example.team=payments` (plan value replaced the Dockerfile's), `com.example.kept=yes`, `created` | `created`, `revision` | none (not sent); the archive descriptor's `created` was BuildKit's export time |
| `linux/amd64,linux/arm64` | same, on both platforms | `created`, `revision` on both | `created`, `revision` |

`docker` exists on the host, but `skopeo` and `buildctl` do not; inspection used Samuori's own archive reader. Not
exercised: Railpack's gateway frontend, Buildah, and a build through `Provider.Run` on a worker.
