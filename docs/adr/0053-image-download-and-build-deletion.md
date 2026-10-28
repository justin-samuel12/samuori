# 0053. Downloading images and deleting builds

Status: **Accepted** (2026-10-27, owner decision; provisional, may change during acceptance testing). Extends
[ADR 0003](0003-separate-build-validation-publication-lifecycles.md) and [ADR 0024](0024-untrusted-content-placement.md).

## Context
A built image could leave Samuori only through a publication to an administrator-defined destination, and nothing could
be removed: every build's image, logs and cache stayed in internal storage for good. Developers asked to try an image
locally before publishing it and to remove builds they no longer need.

## Options
- **Download:** validated images only (chosen); any built image (rejected: an image that failed a secret or policy
  scan could leave Samuori); no download, with a scratch registry instead (rejected as too heavy for a quick local test).
- **Delete:** remove the image, logs and cache and keep the record (chosen); remove the build entirely (rejected:
  breaks links and history); no delete, retention only (rejected for now; retention can come later).

## Decision
- **Download** (`GET /artifacts/{id}/download`): the image's OCI archive with Docker's `manifest.json` added (amended
  2026-10-27: Docker's classic image store refused the plain OCI archive), so `docker load` and `podman load` accept
  it and tag a single-platform image `<project>/<app>:<build version>`; blobs are unchanged. For project maintainers, publishers and administrators, and for developers of the project when the organization's
  `publication` policy sets `developerDownloads` (amended 2026-10-27, owner decision; off by default, set by an
  organization administrator under Registries → Image downloads, shown to the UI through `/me`). Only when the newest validation completed with
  `passed`, the image is not restricted (restricted images leave only through destinations that accept them, ADR
  0031) and no compromised worker built it (ADR 0026). Every download is audited (`image.download`). Publication stays
  the governed way to distribute images; a download carries no signature or provenance.
- **Delete** (`POST /builds/{id}/deletion`, optional reason): project maintainers and administrators, for finished
  builds only. Refused while a publication other than a failed or blocked one, an unreleased hold or a running
  validation uses the image. It records `build_run_deletions` (migration 00065), removes the build's cache entries
  and live output, and is audited (`build.delete`). The build, attempts, validations and audit trail stay; the build
  page shows who deleted it and when.
- **Afterwards:** garbage collection no longer keeps the build's artifacts, nor the images of its failed or blocked
  publications, so the space is freed at the next run unless another build references the same content. A deleted
  build's image is not listed, published, rescanned, converted or downloaded, and its logs answer `410 Gone`.

## Consequences
- A validated image can reach a laptop without a signature; the audit log records who downloaded what.
- Deletion is not reversible. Validation records and their outputs (such as SBOMs) remain as evidence.
- Reversible: removing download is one endpoint; removing deletion leaves `build_run_deletions` unread.
