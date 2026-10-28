# Recipes and Policies

Status: **Authoritative** (implemented in WP10). Related: D16, ADR 0028, `buildspec.md`, `build-execution.md`.

## Recipes

Built-in recipes are versioned YAML definitions embedded in the release (`internal/recipes/catalog/<id>-<version>.yml`),
in the same declarative format as custom recipes plus their runtimes and suggested default commands; the worker
renders them with the custom-recipe renderer (`build-execution.md#recipe-rendering`). Each lists the languages and
application types it supports, its platforms, and its runtimes. Every runtime names a build
image and a run image (optionally one per application type), and **every image is pinned by digest**. Loading fails
if any image is unpinned. Digests and platforms were resolved from the upstream registries on 2026-10-19.

| Recipe | Runtimes | Platforms |
|---|---|---|
| `node-service` 1.0.0 | Node.js 22, 24 | linux/amd64, linux/arm64 |
| `python-service` 1.0.0 | Python 3.12, 3.13 | linux/amd64, linux/arm64 |
| `dotnet-service` 1.0.0 | .NET 8.0, 9.0 (ASP.NET Core for web, .NET runtime for workers and CLIs) | linux/amd64, linux/arm64 |
| `static-site` 1.0.0 | Node.js 22, 24 build; unprivileged nginx serve | linux/amd64, linux/arm64 |
| `powershell-task` 1.0.0 | PowerShell 7.5 on Ubuntu 24.04 | linux/amd64 only (the upstream tag is single-architecture) |

A runtime may declare `distro` (`alpine`, `debian` or `ubuntu`) when its image tags do not name the distribution;
only system packages use it (`buildspec.md#system-packages`). A spec's `build.recipe.systemPackages` adds OS packages
to the build stage and the final image with the image's package manager, and adds the distribution's egress preset
to the recipe's; images of an unknown distribution, including organization runtimes whose tags name none, are refused
at admission.

A spec's `application.runtime.versionConstraint` (`>=22`, `^3.12`, `~3.12`, `3.13`, `net9.0`, ranges joined with
spaces or commas) selects the newest matching runtime. Built-in versions are approved until an organization
administrator deprecates (admission warns) or withdraws (admission refuses; developers no longer see it) them, with
a recorded reason. The same applies to each runtime a built-in version ships, for example Node.js 22 in
`node-service` 1.0.0 while 24 stays approved ([ADR 0060](../adr/0060-deprecating-and-withdrawing-built-in-runtimes.md));
see below. Both decisions apply at admission and planning; builds already planned or queued keep their plan.

### Organization runtimes ([ADR 0051](../adr/0051-organization-runtimes.md))

Shipped runtimes change only with a Samuori release, but each organization decides whether to keep offering each
one ([ADR 0060](../adr/0060-deprecating-and-withdrawing-built-in-runtimes.md)). Under **Recipes & base images >
Runtimes** an organization administrator can **Deprecate**, **Withdraw** or **Approve again** a shipped runtime, each
with a required reason (`PUT /recipes/{recipeId}/versions/{version}/shipped-runtimes/{label}/status`). Decisions are
stored per organization (migration 00068, `shipped_runtime_status`; no row means approved, and approving again
deletes the row) and audited as `recipe.runtime.status`. A deprecated runtime still builds; admission warns
(`runtime-deprecated`) with the reason, and New image marks it. A withdrawn runtime is hidden from developers in
`GET /recipes` and New image, a range such as `>=22` resolves to the newest runtime still available, and a
constraint that matches only withdrawn runtimes is refused at admission (`runtime-unavailable`) naming the runtime and
the reason. An organization runtime with the same label as a withdrawn shipped runtime stays shadowed, so withdrawing
never swaps in other images.

Organization administrators add runtime versions
to a built-in recipe version under **Recipes & base images > Runtimes**: a label (`26`, `3.14`, `10.0`), a build
image and a run image, both full references pinned by digest and allowed by the base-image policy, and an optional
note. They are stored per organization (migration 00060, `organization_runtimes`), so Samuori updates never overwrite
them. A label may not repeat a shipped runtime or an active added one of that recipe version.

An added runtime is a **draft** until an organization administrator approves it with a reason (the images are
checked against the policy again); any administrator may approve, as for custom recipes. Approved runtimes appear in
`GET /recipes` with `source: organization` and the wizard offers them; drafts and withdrawn runtimes never appear
there. Withdrawing needs a reason, keeps the runtime as history and refuses new builds that ask for it.

A version constraint resolves to the newest matching non-withdrawn **shipped** runtime first, and only when none matches to the
newest matching approved organization runtime, so adding a runtime never changes what an existing spec builds. If a
later release ships the same label, the shipped one wins and the added one is marked superseded. A constraint that
matches only a draft or withdrawn runtime is refused at admission (`runtime-unavailable`) with that reason. The plan
records the images by digest and the runtime's `runtimeId` and `runtimeSource`; rendering is the same as for a
shipped runtime with those images. Each added runtime has one run image for all application types. SEC-SPC-08.

**Adding a runtime, for example Node.js 26 to `node-service` 1.0.0** (Node.js 22 and 24 ship built in):

1. Choose the upstream tag and read its multi-platform index digest:
   `docker buildx imagetools inspect node:26-bookworm-slim` (the `Digest:` line at the top). Check that the
   index lists every platform the recipe builds (`linux/amd64`, `linux/arm64`).
2. Write the full reference: `docker.io/library/node:26-bookworm-slim@sha256:<digest>`. Short names such as
   `node:26` are refused.
3. Make sure the repository is allowed by the `base-images` policy (`docker.io/library/node` is by default). A new
   repository needs a policy change or a base-image request first.
4. Open **Registries & Recipes > Recipes & base images**, choose **Runtimes** on Node.js application 1.0.0, then **Add
   runtime**: label `26`, the build image, and the run image (empty uses the build image). Save the draft.
5. **Approve** it with a reason. Developers can now choose Node.js 26 in New image, or set
   `versionConstraint: "26"`.

To retire it, **Withdraw** it with a reason; specs that ask for `26` are then refused at admission.

### Custom recipes ([ADR 0041](../adr/0041-custom-recipes.md))

Administrators add their own recipes under **Recipes & base images > Custom recipes** as YAML: identity and
platforms, typed **parameters** (string, version, port, path), the **secrets** and **egress presets** it needs, and
**stages**, each from an image pinned by digest or an earlier stage, with steps from a fixed set (`workdir`,
`copySource`, `copyFromStage`, `run`, `env`, `user`, `expose`, `label`, `cmd`). `internal/recipedef` validates the
definition and renders it; every value is checked after parameter substitution and every command is written in JSON
exec form, so neither the recipe nor a parameter can add instructions. Shell steps read parameters as build
arguments (`$name`), never as pasted text. `run` steps have no network unless marked `egress`, and then reach only
the recipe's presets. The final stage must end as a non-root user unless the recipe records a `runsAsRoot` reason.

A version is a **draft** until an administrator approves it with a reason, after the **Preview** shows the
Dockerfile Samuori will build; approved versions never change (a change is a new version) and can be deprecated or
withdrawn like built-ins. Developers choose approved versions in New image and fill in their parameters; the spec
records them in `build.recipe.parameters`, admission checks them and the recipe's secrets, and the plan carries the
definition and resolved values so the worker renders exactly what was previewed. Stage images go through the
base-image and image-source policies. SEC-SPC-06 (rendering) and SEC-SPC-07 (lifecycle).

Not yet: a structured form editor, trial builds from the recipe page, and shared reusable steps.

## Policies

Each organization has one versioned policy per kind. Every change creates an immutable version and is audited.
Edits must send the version they read in `If-Match`; a stale version gets `412`. Defaults are created the first
time an organization is used.

| Kind | Document | Default |
|---|---|---|
| `frontends` | `approved`: Dockerfile frontends allowed in `# syntax=`, each `name@sha256:<64 hex>` | none approved |
| `base-images` | `allowedRepositories` (no tag or digest), `requireDigest`, `pinTags` (needs `requireDigest`) | the built-in recipes' repositories; digests required; tags pinned at admission for new organizations (stored policies without the field keep `false`) |
| `build-args` | `rules`: `id`, `namePattern` and/or `valuePattern` (regular expressions), `reason` | no rules |
| `egress` | `allowedPresets`, `dockerfilePresets`, `destinations` (`egress.md`) | every preset allowed; Alpine, Debian and Ubuntu for Dockerfile builds; no destinations |
| `validation` | `requireSbom`, `requireRuntimeTest` (`validation.md`) | SBOM optional, runtime test required |
| `sensitive-content` | `outcomes` per rule class, `extraRules` (`id`, `class`, relative `path` glob, `reason`) | ADR 0031 defaults; delivered secret bytes always block |
| `publication` | `developerDownloads`: developers may download validated images of their projects (ADR 0053) | off |

Discovery and admission read the same policies. Other kinds in the API (`security`, `tags`) are refused with `unsupported_policy` until their components exist.

`POST /policies/{id}/dry-run` evaluates a proposed document without saving it and lists the specs it would newly
block. Recipe specs are checked from the spec alone; Dockerfile specs are checked against each project's newest
upload.

**Confirmation for changes with impact (TM-92, INT-ADM-01).** When a dry run finds specs the change would newly
block, it also returns a `confirmation` token. The token is valid for 15 minutes and only for that policy, its
current version and that exact document. `PUT /policies/{id}` recomputes the impact itself. If anything would be
newly blocked, it applies the change only with `X-Samuori-Confirm: <token>` and a `reason` of at least 10 characters,
which is kept in the `policy.update` audit record; otherwise it answers `428 confirmation_required` with the impact.
A change that blocks nothing needs neither. The Security & Secrets > Network egress page shows the impact and asks
for the reason before applying such a change.

### Tag pinning

With `requireDigest` and `pinTags` (ADR 0049), a Dockerfile `FROM` tag from an approved repository, such as
`FROM node:18-alpine`, is accepted: admission resolves it on the controller to the digest the tag names now (the
index for a multi-platform image), with an anonymous registry `HEAD` through the address guard and a 20-second
timeout, and the build plan records `node:18-alpine → docker.io/library/node@sha256:…` under
`dockerfile.baseImagePins`. The worker builds that digest (`build-execution.md#pinned-base-images`); the build page
lists the pins. A tag that cannot be resolved blocks the build with the registry's answer. Stage references,
`scratch` and `ARG`-based images are never pinned; images in named contexts, a `FROM` that a named context or stage
of the same name would replace, and tags from private upstreams with approved image sources are refused with a
reason to pin by digest. With `pinTags` off, a tag is refused and the reason says an administrator can turn pinning
on. Registries & Recipes > Recipes & base images has the switch ("Pin tags to their current digest at admission"); saving
uses the same dry run and confirmation as other policies, and the impact preview treats pinnable tags as allowed
without querying registries. Samuori records the digest; it never updates base images.

### Base-image requests

Developers and maintainers cannot edit policies, so they ask for a repository instead:
`POST /projects/{id}/base-image-requests` with `repository` and a `justification` of at least 20 characters;
`GET` lists the project's requests with their state, the decider's reason and, once approved, the policy version
that lists the repository. The repository is checked like a policy entry (a lower-case registry host and path, no
tag, digest or port) and short Docker Hub names are expanded (`node` becomes `docker.io/library/node`). A
repository already on the list is refused with `409 already_allowed`, and a second open request for the same
repository in the organization with `409 request_pending`; a unique index on the open repository holds this under
concurrent requests.

Each request creates a `base-image-request` approval that only organization-scoped administrators decide, never the
requester (`approvals.md`). Approval appends the repository to the current `base-images` version inside the deciding
transaction: it bumps the policy version first, which takes the row lock, then extends the document of the version
before it. An administrator's concurrent edit therefore either lands first and is extended, or fails its
`If-Match` afterwards; nothing is lost. The new version's `policy.update` audit record names the request, and the
approval decision is audited as usual. `requireDigest` and `pinTags` are unchanged.

When admission blocks a base image only because its repository is unlisted, the `base-images` decision lists it in
`requestableRepositories` and the reason says it can be requested; New image shows a request button for each.
Approval changes policy only: a registry that has approved image sources still needs one that allows the project
(`registry-auth.md`), and a strict-egress worker reaches a registry other than docker.io only through a mirror
(`egress.md#mirrors-for-other-registries`).

## Admission dry run

`POST /specs/{id}/revisions/{n}/admission-checks` with a snapshot reports, without creating a build:

- schema, semantic and argument-policy errors, and secret-looking literals anywhere in the spec;
- recipe existence and status, language, application type, runtime and platform support;
- for Dockerfiles: context, Dockerfile and named-context paths in the snapshot, a fresh sandboxed analysis, the
  frontend policy, refused features (`ssh` mounts, remote `ADD`), blocking ambiguities (base image or secret ID from
  a variable), target stage, and secret mounts reconciled with declared secrets;
- the base-image policy for every recipe or Dockerfile base image and image named context, with the tags it would
  pin (`baseImagePins`; a build resolves them again when it is admitted);
- the egress policy: the recipe's presets (or the Dockerfile presets) must be allowed;
- engine platform support (BuildKit: linux/amd64, linux/arm64);
- secret readiness. Secret bindings arrive in WP16, so every declared secret reports `missing` and a required
  secret makes the revision inadmissible until then.

Worker-pool eligibility and destination authorization join admission with the scheduler (WP13) and publication
(WP21).
