# 0049. Base-image tag pinning at admission

Status: Accepted (2026-10-26) — owner decision. Implementation: built 2026-10-26 with focused tests (policy,
admission, plan digest, BuildKit context names, scheduler engine check, anonymous registry lookup against a local
token-protected test registry); no real BuildKit build of a pinned plan has run yet.

## Context
A Dockerfile build of a real application was blocked with "Base images not allowed: node:18-alpine (it is not
pinned by digest)". The `base-images` policy could either require digests, which makes developers edit every `FROM`
line, or accept tags, which builds whatever the tag points to when the worker pulls it and leaves no record of it.

## Decision
- **A third setting.** The `base-images` policy gains `pinTags`. With `requireDigest: true` and `pinTags: true`, a
  `FROM` tag from an approved repository is accepted and pinned; with `pinTags: false` the refusal stays, and its
  reason says an administrator can turn pinning on. `pinTags` without `requireDigest` is invalid. Stored policies
  without the field keep `false`; the default document for new organizations sets it to `true`.
- **Resolved at admission, on the controller.** Each unpinned `FROM` image is resolved with an anonymous registry
  `HEAD` of its manifest (Docker Hub's token flow included, `registry-1.docker.io` for `docker.io`), through the
  address guard and a 20-second timeout. The digest pinned is whatever the tag names: the index for a multi-platform
  image. A missing tag, a network failure or no resolver blocks the build with a reason; the tag is never built
  unpinned.
- **Recorded in the plan.** The pins (`ref` as written, `image` as `repository@sha256:<digest>`) are in
  `dockerfile.baseImagePins` of the build plan, so the plan digest covers them and a re-run of the plan builds the
  same images. `GET /builds/{id}` returns them and the build page shows them. The admission dry run shows them too,
  but a build resolves again when it is admitted.
- **Built with BuildKit named contexts.** The worker passes `context:<name>=docker-image://<image>` for each pin. The
  user's Dockerfile and BuildSpec are not changed. `<name>` is what BuildKit v0.34.0's Dockerfile frontend looks up:
  `resolveBaseImage` (`frontend/dockerfile/dockerfile2llb/convert.go`) normalizes the `FROM` name with its tag, and
  `dockerui.Client.NamedContext` (`frontend/dockerui/config.go`) reduces it to the familiar form without `:latest`, so
  `node:18-alpine` and `docker.io/library/node:18-alpine` both match `node:18-alpine`, and `node` and `node:latest`
  match `node`. Buildah has no equivalent Samuori relies on, so pinned plans go only to BuildKit workers and a Buildah
  worker refuses one.
- **Not pinned:** stage references and `scratch` (never base images), `ARG`-based `FROM` lines (already blocking
  ambiguities), images in user-declared named contexts, and a `FROM` whose name a user named context or a stage
  name would replace in BuildKit (refused with a reason to pin by digest). Tags from a private upstream with an
  approved image source (ADR 0030) are refused with a reason to pin by digest; resolving them would need the
  mirror's credentials on the controller.
- **Samuori does not maintain base images.** It records which digest a tag named at admission; it does not track or
  update tags.

## Consequences
- Developers keep `FROM node:18-alpine`; two builds of the same spec can use different digests when the tag moved in
  between, and each build records which one it used.
- The controller needs outbound HTTPS to the public registries it pins from. Registries on loopback or private
  addresses are not reached (SEC-NET-03); images from them must be pinned by digest. The impact preview of a policy change
  does not query registries; it treats pinnable tags as allowed.
- Workers that build pins announce worker protocol 1.5; the scheduler gives a pinned plan only to workers that
  negotiated 1.5 or later, because older ones would build the moving tag. Until the workers are updated, a pinned
  build waits in the queue.
