# BuildSpec and Build Plan

Status: **v1alpha1 schema drafted** at `api/schemas/buildspec.v1alpha1.json` (JSON Schema 2020-12), validated against
fixtures that were removed with the tests (ADR 0039; `docs/developers/testing.md`). The field tables below are
authoritative; the schema implements them. Build methods and secrets follow ADRs 0028
and 0029 (accepted as direction, OD-13). The API group is a placeholder (ADR 0008).

## Principles

- **Declarative intent, not a Dockerfile replacement.** The spec says what to build and under which constraints. For
  the `dockerfile` method, instructions stay in the user's Dockerfile.
- **Portable.** No source snapshot reference (ADR 0004). No secret values, provider paths, AWS account IDs, IAM
  roles, credentials, internal storage choices, worker assignments or runtime state. Those live in run and admin
  records.
- **One model, three views.** Guided, Detailed and YAML edit the same AST (ADR 0005). The schema is strict
  (unknown properties are rejected) except for `x-*` extension fields at `spec` level. Those are the
  "unknown-but-allowed" fields that every view preserves. Secrets never accept extensions. Constructs not representable in Guided mode are shown read-only with a switch-to-Detailed action.
- **Original and provider-neutral.** It mirrors no prior project's specification.
- **Safe execution.** User commands are executed as argument arrays through engine APIs, never concatenated into a
  host shell. Serialization and previews never contain credentials.

## Top level

| Field | Required | Notes |
|---|---|---|
| `apiVersion` | yes | `samuori.invalid/v1alpha1` |
| `kind` | yes | `ImageBuild` |
| `metadata.name` | yes | DNS-label style |
| `metadata.description`, `metadata.labels` | no | Labels are spec metadata, not image labels |
| `spec` | yes | Below |

## `spec`

| Field | Required | Notes |
|---|---|---|
| `application` | recipe: yes; dockerfile: no | Detected or selected language and framework, app type, runtime constraint, port, health path. Informational for `dockerfile`, where it drives validation profiles and UI hints |
| `build.method` | yes | `recipe`, `dockerfile` (ADR 0028), `railpack` or `buildpacks` (ADR 0047) |
| `build.recipe` | when `recipe` | `id`, `version`, and commands (`install`, `build`, `start`, `test`) as shell strings or argument arrays (an array is one command's argv, not a list of commands); `systemPackages` (below) |
| `build.dockerfile` | when `dockerfile` | `path` (relative to `context`), `context` (relative path in the source), `target` (stage), `namedContexts` (name → relative path or approved image reference) |
| `build.railpack` | no; only with `railpack` | `context` (relative path, default `.`), `buildCommand`, `startCommand` (single-line strings passed to `railpack prepare --build-cmd`/`--start-cmd`; they run in build steps, never during planning). Nothing else; the project's own `railpack.json` still applies |
| `build.buildpacks` | no; only with `buildpacks` | `context` (relative path, default `.`); `env`: non-secret buildpack configuration, names `BP_*`, `BPE_*` or `BPL_*` (`BPE_`/`BPL_` reach the image), single-line values, at most 64; `bindings`: CNB service bindings, each `name`, `type`, `secret` (a declared build secret's logical ID) and optional `key` (the file name, default the secret ID; not `type` or `provider`). The project's own `project.toml` still applies |
| `build.args` | no | Non-secret build arguments: `name`, `value`, `confirmedNotSecret`. Secret-looking names require `confirmedNotSecret: true`. That flag is a **user declaration, not a security guarantee**: admin policy can reject any argument by name or value pattern regardless of it, and values remain visible in image history |
| `build.engine` | no | Optional preference (`buildkit`). Policy decides |
| `secrets` | no | Logical secret declarations (see below) |
| `target.platforms` | no | OCI platform strings. Defaults from recipe or policy. Validated against engine capabilities before scheduling |
| `validation` | no | `profile` (`http-startup`, `process-start`, `cli-smoke`, `static-serve`, `none-requested`), extra checks, timeouts. Admin-required checks are added by policy and cannot be removed |
| `outputs.image` | no | `labels` (image config) and `annotations` (image manifest) the build applies, merged with automatic OCI values ([below](#image-labels-and-annotations)); `tags`, the tags a publication applies when the publisher gives none: OCI tags or `{version}` for the build's version, at most 16, no `samuori-` prefix, default `["{version}"]` (ADR 0054) |
| `outputs.sbom`, `outputs.provenance` | no | Requested attestations. Policy may require them |
| `outputs.expectedSensitiveContent` | no | `path` plus `reason` entries that explain intentional sensitive outputs (ADR 0031). This does not grant an exception |
| `publication` | no | `mode` (`manual` / `automatic`), logical `destinationRef`, `requestedTags`. Never credentials. `destinationRef` must resolve to an authorized, admin-managed destination at admission, and the controller re-validates authorization when the publication actually runs (`gate_check`) |

### `secrets[]`

| Field | Required | Notes |
|---|---|---|
| `id` | yes | Matches `--mount=type=secret,id=…` or the recipe's reference. Pattern `^[a-z0-9][a-z0-9_.-]{0,62}$` |
| `type` | no | `text` (default) or `binary` |
| `required` | no | Default `true` |
| `usage` | no | `build` (default) or `test` |
| `mount.target` | no | Expected path. For `dockerfile` it must match the detected target. Default `/run/secrets/<id>` |
| `description` | no | Shown to admins when they create a binding |

Values, provider paths and binding IDs are never valid here. The schema rejects unknown properties under each
`secrets[]` item.

## Examples

### Beginner, generated recipe

```yaml
apiVersion: samuori.invalid/v1alpha1
kind: ImageBuild
metadata:
  name: orders-api
spec:
  application:
    type: http-service
    runtime: { language: nodejs, versionConstraint: ">=22" }
    port: 3000
  build:
    method: recipe
    recipe:
      id: node-service
      version: "1.0.0"
      install: npm ci
      build: npm run build
      start: ["node", "dist/server.js"]
  target:
    platforms: [linux/amd64]
  validation:
    profile: http-startup
  publication:
    mode: manual
    destinationRef: dev-registry
    requestedTags: ["1.0.0"]
```

### Advanced, existing Dockerfile with build secrets (SQL Server)

```yaml
apiVersion: samuori.invalid/v1alpha1
kind: ImageBuild
metadata:
  name: sales-db
spec:
  application:
    type: database
  build:
    method: dockerfile
    dockerfile:
      context: db
      path: Dockerfile
      target: final
    args:
      - { name: MSSQL_PID, value: Developer }
  secrets:
    - { id: sa_password, type: text }
    - { id: certificates_password, type: text }
    - { id: master_key, type: binary, mount: { target: /certificates/master.key } }
    - { id: tde_cert, type: binary, mount: { target: /certificates/SQLServerTDECert.cert } }
    - { id: tde_key, type: binary, mount: { target: /certificates/SQLServerTDECert.pvk } }
  target:
    platforms: [linux/amd64]
  validation:
    profile: process-start
  outputs:
    expectedSensitiveContent:
      - { path: /var/opt/mssql/data, reason: "Restored reference database shipped with the image" }
      - { path: /var/opt/mssql/secrets, reason: "Service master key required by TDE" }
  publication:
    mode: manual
    destinationRef: restricted-db-images
```

The Dockerfile keeps its own `RUN --mount=type=secret,...` lines unchanged. Samuori binds each logical ID to an
admin-approved secret for this project. Because of the expected sensitive outputs, publication needs an approved
exception and a destination that accepts restricted images (ADR 0031).

### System packages

`build.recipe.systemPackages` lists operating system packages a **built-in** recipe installs: `build` in its build
stage (compilers, headers, `git`), `runtime` in the final image (shared libraries, `ca-certificates`). Each list
holds at most 64 unique names matching `^[a-z0-9][a-z0-9+.-]{0,127}$`, so a name can never carry options, versions
or shell text; the worker checks the names again before rendering.

- **Package manager.** Admission reads each image's distribution from its repository name and tag (`alpine`;
  `debian`, `bookworm`, `bullseye`, `buster`, `trixie`; `ubuntu`, `focal`, `jammy`, `noble`), or from the runtime's
  declared `distro` in the catalog when the tag names none (the .NET images are declared Debian). Alpine uses
  `apk add --no-cache`; Debian and Ubuntu use `apt-get update`, `apt-get install -y --no-install-recommends` and
  remove the package lists. An image whose distribution is unknown, for example an organization runtime tagged
  `node:26`, is refused with `system-packages-unsupported` at the list's path. Custom recipes refuse the field
  (`system-packages-custom-recipe`); they install their own packages.
- **Rendering.** The install is one `RUN` right after the stage's `FROM`, preceded by `USER 0` because some run
  images (unprivileged nginx) default to another user; the recipe's own `USER` step follows, so the final image
  still runs as the recipe's non-root user. The step has network access and no secret mounts. Specs without
  system packages render byte-identically to before (golden test).
- **Egress.** Admission and the plan add the distribution's preset (`alpine`, `debian` or `ubuntu`) to the recipe's
  presets, so the egress policy must allow it.
- **Plan.** `recipe.systemPackages` records the names and the manager chosen for each image; the plan digest covers
  them. A worker of an earlier release reports the plan's unknown field as version skew instead of building without
  the packages. Tested by `TestSystemPackagesSchema`, `TestSystemPackagesPlan`, `TestSystemPackagesAdmission`,
  `TestSystemPackagesRenderChecksThePlan`, `TestDistro`.

### Frontend

`build.recipe.frontend` serves a browser frontend from the same image as a built-in recipe's application, through
Samuori's web launcher `samuori-web` ([ADR 0055](../adr/0055-frontend-and-backend-in-one-image.md)).
`build.recipe.path` is the application's folder (the upload's root when omitted).

```yaml
spec:
  application: {type: http-service, port: 8080, runtime: {language: python}}
  build:
    method: recipe
    recipe:
      id: python-service
      version: 1.0.0
      path: backend
      install: pip install -r requirements.txt
      start: uvicorn app.main:app --host 127.0.0.1 --port 8000
      frontend: {path: frontend, output: dist, apiPrefixes: [/api], backendPort: 8000}
```

- **Fields.** `path` (required), `install` (default `npm ci`), `build` (default `npm run build`), `output` (default
  `dist`, relative to `path`), `node` (a Node.js major version; the newest when omitted), `apiPrefixes` (1 to 8 paths
  such as `/api`; default `/api`) and `backendPort` (default 8000).
- **Rules.** `frontend-http-service`, `frontend-port`, `frontend-port-clash` (the image's port and `backendPort`
  differ), `frontend-start` (the launcher starts the application with its start command), `frontend-one-platform`,
  `frontend-same-folder`; admission adds `frontend-runtime` and `frontend-custom-recipe`.
- **Rendering.** A `samuori-frontend` stage on the Node.js recipe's pinned image copies the folder, runs the install
  command with the build's egress and the build command with `--network=none`. The final stage copies the output to
  `/srv/frontend` and `/samuori-web` from the worker's `samuoriweb` named context, and its command is
  `samuori-web --listen :<port> --root /srv/frontend --proxy <prefix>... --backend http://127.0.0.1:<backendPort> --
  <start command>`. The launcher serves files with a fallback to `index.html`, forwards the prefixes, forwards stop
  signals to the application and exits with its status. Specs without a frontend render byte-identically to before.
- **Plan.** `recipe.path` and `recipe.frontend` (with the Node.js image, the resolved defaults and the launcher
  contract) are covered by the plan digest; the scheduler sends the plan only to workers announcing the `samuori-web`
  provider. Tested by `TestFrontendRules`, `TestFrontendPlan`, `TestFrontendRendersLauncherImage` and the
  `internal/webrun` tests.

### Image labels and annotations

`outputs.image.labels` go into the image configuration (what `docker inspect` shows); `outputs.image.annotations`
go onto the image manifest (what registries show). Neither is a tag. Anyone who can pull or inspect the image reads
both, so they must never hold secrets.

- **Validation** (`internal/buildspec`, semantic rules `image-metadata-key`, `image-metadata-value`,
  `image-metadata-count`; the schema bounds the maps). At most 64 entries per map. Keys are reverse-DNS, at most 255
  bytes, matching `^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(\.[a-zA-Z0-9][a-zA-Z0-9._/-]*)*$`: a lowercase domain-style head
  as Docker's label guideline asks, then dot-separated parts that may use mixed case, `_` and `/`, as OCI and
  common tooling keys do. This also keeps keys free of the `[`, `]` and whitespace that BuildKit's attribute syntax
  would read as a platform qualifier or separator. Keys starting with `io.samuori.` (or `io.samuori` itself) are reserved
  for Samuori. Values are at most 4 KiB and contain no control characters (no line breaks or tabs). A value in a known
  credential format is a `secret-literal` error like any other spec string.
- **Automatic values.** For every build the plan adds `org.opencontainers.image.created` (the BuildRun's admission
  time, RFC 3339 UTC), `org.opencontainers.image.title` (`metadata.name`, the image name the wizard asks for) and,
  only when the snapshot was fetched by a pipeline from a known Git commit, `org.opencontainers.image.revision`
  (that full commit hash; uploads have none and get none). They are applied both as labels and as annotations,
  since the OCI image spec defines the same keys for both. A spec value for the same key wins. Samuori has no
  reproducible-build mode and does not set `SOURCE_DATE_EPOCH`, so `created` makes every admission a different plan
  and image digest; a retry of the same BuildRun reuses its plan and so the same value.
- **Methods.** `dockerfile` and `recipe` builds apply both. `railpack` applies annotations only: labels would have
  to reach Railpack's gateway frontend, which is not known to honour them. `buildpacks` applies neither, because the
  CNB lifecycle exports the image. Admission warns `image-labels-not-applied` or `image-annotations-not-applied`
  when the spec sets what the method does not apply, and the plan carries only what is applied.
- **Secrets.** Admission warns `image-metadata-secret-like` for a key that looks like a credential name (the same
  heuristic the sensitive-content check uses for environment variables) or an entry that matches the build-args
  policy's name or value patterns. After the build, the sensitive-content check's metadata class flags such labels
  and annotations ([`validation.md`](validation.md#sensitive-content)).
- **Plan.** `image.labels` and `image.annotations` hold the merged values; the plan digest covers them. Such plans go
  only to workers on worker protocol 1.6 or later. Tested by `TestImageMetadataValidation`, `TestImageMetadata`,
  `TestImageMetadataInPlanDigest`, `TestImageMetadataNeedsProtocol16` and `TestImageMetadataWarnings`.

## Rules enforced outside the schema (semantic validator)

These cannot be expressed in JSON Schema. The semantic validator is `internal/buildspec`. The first three rules,
the argument policy and secret-literal rejection are implemented and tested (SEC-SPC-02, SEC-SPC-04, SEC-SPC-05).
Secret reconciliation, recipe checks, snapshot path checks and engine platforms are enforced by admission
(`internal/admission`, `recipes-and-policies.md`); pool eligibility and destinations arrive with their components. The first three rules **must be implemented and passing
before any BuildSpec is used to authorize a real build** (M3 gate):
- unique `secrets[].id` and unique `build.args[].name`;
- Windows platforms rejected at admission until OD-7b, while the schema stays platform-aware;
- spec secrets reconciled with secret mounts detected in the Dockerfile (ADR 0028);
- admin argument policy (name and value patterns), applied regardless of `confirmedNotSecret`;
- recipe exists, is approved and is not deprecated beyond policy;
- `context`, `path` and named-context paths exist in the source snapshot;
- platforms supported by the engine and an eligible pool;
- `destinationRef` and approved image sources authorized for the project;
- `railpack`: no `build.args` (`railpack-build-args`; the frontend ignores them), one platform (`railpack-one-platform`),
  build secrets reach steps as the environment variable `RailpackSecretEnv(id)` (upper case, `-` and `.` → `_`), so two
  IDs may not map to one variable (`railpack-secret-env-clash`), none may map to a proxy variable
  (`railpack-secret-env-reserved`) and none may set `mount` (`railpack-secret-mount`). Tested by
  `TestRailpackBuildSpec`. Admission adds the provider and placement checks (`build-execution.md#railpack-builds-adr-0047`).
- `buildpacks`: no `build.args` (`buildpacks-build-args`; use `env`), one platform (`buildpacks-one-platform`), every
  build secret bound by a binding (`buildpacks-secret-unbound`), every binding naming a declared build secret
  (`buildpacks-binding-secret-unknown`), no secret `mount` (`buildpacks-secret-mount`), one type per binding name
  (`buildpacks-binding-type-clash`), one secret per binding file (`buildpacks-binding-duplicate`) and no file named
  `type` or `provider` (`buildpacks-binding-key-reserved`). Tested by `TestBuildpacksBuildSpec`. Admission adds the
  provider and placement checks (`build-execution.md#buildpacks-builds-adr-0047`).
- `outputs.image`: label and annotation keys, values and counts ([above](#image-labels-and-annotations)).

## Build plan

At admission the server resolves the spec into an immutable **build plan**. It contains:
- source snapshot digest and spec revision digest;
- method (`recipe` with its generated Dockerfile digest, `dockerfile` with the path and the digest of the
  analysed Dockerfile, or `railpack` with the pinned provider: version, binary checksums, frontend and image digests,
  context, commands, cache key, secret variable mapping and time limit, or `buildpacks` with the builder and run
  image digests, the CNB platform API, lifecycle version, context, `env`, bindings by logical ID and time limit);
- approved frontend and base-image digests, including `FROM` tags pinned at admission (`dockerfile.baseImagePins`,
  ADR 0049);
- the image labels and annotations the build applies (`image`, [above](#image-labels-and-annotations));
- recipe version;
- engine and isolation profile;
- logical secret IDs with binding **versions**, never values or provider paths;
- mirror and egress policy versions;
- the versions of every policy that applied.

The plan digest covers all of these. The plan is immutable, delivered only over the worker's mTLS channel and
verified against the digest bound to the lease (ADR 0014). The source snapshot ID belongs to the BuildRun input,
not to the reusable spec.

## Canonical form and digest

Implemented in `internal/buildspec` (`Canonical`, `Digest`).
- **Canonical YAML:**
  - block style, two-space indentation;
  - top level in the order `apiVersion`, `kind`, `metadata`, `spec`;
  - `spec` fields in schema order, with `x-*` extensions last, sorted by key;
  - maps sorted by key;
  - empty optional fields omitted.

  It is idempotent and always re-validates. The YAML editor view and stored revisions use it, so diffs between
  revisions are stable.
- **Digest:** `sha256:` over the canonical JSON encoding. Two documents that differ only in YAML formatting have the
  same digest. Spec revisions and build plans reference this digest.

## Editor implementation

- The browser keeps one JSON document. The form (ADR 0005 as amended 2026-10-26: one form plus YAML) reads and
  writes paths in it (`formPaths` in `ui/src/spec/model.ts`) and never drops fields it does not show; those are
  listed as YAML-only (x-* extensions, `outputs.expectedSensitiveContent`, buildpacks `env` and bindings). Install,
  build and test commands are edited as ordered lines: one line is saved as that string, several as one shell string
  joined with ` && ` (`ui/src/spec/lists.ts`), since an array is one command's argv. Commands written as argument
  lists or as multi-line scripts are locked outside YAML, so the form never rewrites them.
- `POST /spec-documents/validate` parses YAML or JSON, validates it with the organization's policies and returns the
  canonical YAML, normalized document and digest without saving anything. The YAML view always shows this server
  rendering. Documents that fail the schema are still rendered (keys sorted) so incomplete drafts can be edited.
- The editor records which view last changed each field and asks before the other view overwrites it. Stored
  drafts from the former Guided and Detailed modes open in the form.
- Wizard progress is saved per user and project (`/projects/{id}/wizard-draft`, optimistic versioning, at most
  256 KiB). Drafts containing credential-like text are refused; drafts never hold secret values.
- Saving creates a spec or a new revision with `If-Match`; the review step runs the admission dry run against the
  chosen upload.
- Spec names are unique per project. Creating a spec under a taken name returns `409 spec_exists`; the wizard then
  finds that spec (`GET /projects/{id}/specs?name=`) and offers to save the work as its next revision, which goes
  through the same revision endpoint, role check, `If-Match` and `spec.revision` audit event as any other edit.

## Suggested specs

`GET /snapshots/{snapshotId}/spec-suggestions` (`listSpecSuggestions`, developer or maintainer of the snapshot's
project; another team's snapshot is 404) turns the snapshot's newest completed discovery into starting specs, so the
CLI (`samuori spec init --snapshot`) and scripts start where the web wizard does. It reads the stored discovery
result only: nothing in the upload runs, nothing is saved, and `409 discovery_required` (or `discovery_running`) means
discovery must finish first. Each suggestion is rendered by the same `specs.Render` as `/spec-documents/validate`
(canonical YAML, normalized JSON, digest, schema and semantic issues) and carries its method, a title, the reasons for
its choices and warnings. The list holds, in this order, at most 20 items:

- a **recipe** spec per detected application whose language and application type an approved recipe covers (first
  match in the catalogue order of `GET /recipes`), with type, runtime, version constraint, commands and folder from
  the finding and the validation profile for the type. An application without an approved recipe gets a `no-recipe`
  note in the list's `warnings` instead;
- a **frontend and backend** recipe spec for each Node.js static site and HTTP service in separate folders (ADR 0055):
  port 8080 for the image, the application on 8000 behind `/api`;
- a **dockerfile** spec per analysed Dockerfile: built as written, stopping at the last stage, with the type and
  runtime of the application found in its folder and the port when it exposes exactly one;
- **railpack** and **buildpacks** specs for the first application, building its folder.

Methods the project may not use (`GET /projects/{id}/build-methods`) are left out. The item the wizard would
recommend is marked `recommended` and listed first: the organization's default method when it fits, else the first
Dockerfile, a recipe, then Railpack. Warnings repeat the finding's or Dockerfile's discovery warnings and readiness
(for example `no-lockfile` or `install-scripts`). Discovery finds neither listening ports nor health paths, so when the
`http-startup` check needs a port and none is known, the suggestion assumes 3000 for Node.js, 8000 for Python and 8080
otherwise and says so with a `port-assumed` warning; no health path is set.

The derivation is in `internal/specs/suggest.go` and mirrors the wizard's `fromFinding`, `pickRecipe`, `appName`,
`withFrontend` and `frontendBackendPairs` (`ui/src/spec/model.ts`). The two are kept in step by one fixture,
`internal/specs/testdata/suggestions.json`, that both `TestSuggestMatchesWizard` and `model.test.ts` check. The only
deliberate differences are the assumed port and that Railpack and Buildpacks suggestions build the application's
folder rather than the upload's root.

## Versioning and migration

- Every stored revision keeps the `apiVersion` it was written with. Revisions are immutable and never rewritten in
  place.
- Each new schema version gets:
  - its own schema file (`api/schemas/buildspec.<version>.json`);
  - its own fixture set;
  - a pure, deterministic migration from the previous version.
- Migrations chain (for example v1alpha1 → v1beta1 → v1). Each one has fixture-based tests: every valid fixture of
  the old version migrates to a valid document of the new version, and the semantics are checked against
  hand-written expectations.
- Reading an older revision migrates it in memory. Saving writes a **new** revision in the current version, and only
  after the user confirms the shown diff. Admission accepts any supported version and records the migrated digest in
  the build plan.
- A migration that would drop or reinterpret information fails with an explanation and does not guess. `x-*`
  extensions are carried across unchanged.
- The first migration will replace the provisional `samuori.invalid` group with the controlled domain (OD-1).
- No migration code exists until a second version exists, to avoid speculative code.
