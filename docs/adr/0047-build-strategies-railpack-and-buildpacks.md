# 0047. Build strategies: Railpack and Cloud Native Buildpacks

Status: Accepted (2026-10-24) — owner directive "Unified Build and Kubernetes Conversion Integration". Amends ADR
0028 (build methods) and D16 (recipes stay; they are no longer the only automated method). Implementation in progress
(roadmap WP45): the `railpack` method is built (`../architecture/build-execution.md#railpack-builds-adr-0047`,
`../spikes/railpack-builds.md`), and so is `buildpacks`: the lifecycle runs as a BuildKit build from the approved
builder, with the run image also pinned by digest (`../architecture/build-execution.md#buildpacks-builds-adr-0047`,
`../spikes/buildpacks-builds.md`). Making Railpack the default automated strategy needs a separate decision.

## Context
Samuori's automated builds come from five built-in recipes, Samuori's own command heuristics and a renderer with a branch per
recipe (`internal/engine/render`). Railpack and Paketo Buildpacks already detect, plan and build applications and
track their ecosystems upstream. The 2026-10-19 spike (`../spikes/railpack.md`) showed Railpack planning runs a
downloaded `mise` but never the application; CNB detection runs buildpack binaries. Neither may run during discovery
(`../architecture/discovery.md`, TM-16).

## Decision
- **Discovery is unchanged.** The static detectors stay the only discovery code and recommend a strategy; Railpack and
  CNB never run on the controller or in the discovery sandbox.
- **Two new build methods** beside `recipe` and `dockerfile`, selected in the BuildSpec and the wizard:
  - `railpack`: on the worker, inside the build isolation, `railpack prepare` writes the plan with egress only to
    approved sources (or offline from an administrator-staged, digest-verified mise cache). Samuori rejects an empty or
    partial plan whatever the exit status, records the plan and `railpack-info.json`, then builds it with rootless
    BuildKit through the `gateway.v0` frontend `ghcr.io/railwayapp/railpack-frontend`, pinned by digest and matching
    the Railpack version. Cache mounts are prefixed per project (`cache-key`); secrets are BuildKit secrets by logical
    ID, as for Dockerfiles.
  - `buildpacks`: the CNB lifecycle (`creator`) from an administrator-approved builder image pinned by digest runs
    inside the build isolation and writes an OCI layout; the worker treats the layout as the build result. Secrets
    become CNB bindings materialized for the step only. Buildpack metadata and the lifecycle SBOM are kept as
    artifacts.
- **The same controls apply to every method:** isolation profile eligibility, the egress proxy and allowlist,
  mirrors, logical secrets, per-build caches, limits, validation, the sensitive-content and non-root checks, SBOM,
  signing, lineage and publication policy. A method whose requirements an isolation profile cannot meet is refused
  at admission with the reason. Per-step network denial that Samuori's own recipe renderer adds does not exist inside a
  third-party plan; the build's egress allowlist and the post-build checks carry that duty.
- **Providers are optional and administered.** Railpack and CNB are disabled until an administrator approves a
  version (Railpack binary and frontend digest; lifecycle and builder digests), allows them per pool or project and
  sets the allowed and default methods. Binaries arrive with the release or from an administrator-approved, digest-
  pinned source, never from a user-supplied URL. A basic installation needs neither.
- **Recipes stay.** Built-in recipes are re-expressed in the declarative custom-recipe format (`internal/recipedef`)
  so one renderer serves both, with byte-identical output. Existing BuildSpecs are not migrated.

## Amendment (2026-10-26, owner decision): non-root Railpack images
Railpack v0.43.0 images run as root (`User=""`); validation ran them as uid 10001 so they passed, but the published
image ran as root. Samuori now adds a non-root user layer to Railpack images by default: the plan's `railpack.user` is
`10001:10001`, and after the build the worker appends to each root image one layer that owns the working directory and
the directories below it as 10001:10001 and sets `User` to it (`internal/adapters/isolation/rootlesshost/nonroot.go`,
`../architecture/build-execution.md#railpack-builds-adr-0047`). Files stay root-owned and read-only to the
application. The step reads layers as data and runs nothing from the image. It is a provider setting, reversible per
organization: `runAsRoot: true` on the Railpack provider keeps Railpack's root user, and plans made before this change
(no `user`) build as before. Rejected: `chown -R` in a build step (copies every file into a new layer and runs a
binary from the image) and changing Railpack's plan (its format has no user field in v0.43.0).

## Consequences
- Samuori maintains less language-specific build logic as new projects move to Railpack or buildpacks.
- A Railpack or buildpacks image is only as constrained as its plan plus Samuori's worker and post-build controls.
- Reproducing a build needs the pinned Railpack, frontend, lifecycle and builder versions to remain available.
