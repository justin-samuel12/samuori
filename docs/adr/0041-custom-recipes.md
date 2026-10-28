# 0041. Custom recipes

Status: **Accepted** (2026-10-21, owner decision). Implements D16 ("admin-maintained, approved, versioned
built-in/custom recipes, base image policies and reusable steps").

## Context
Built-in recipes (`internal/recipes/catalog/`) give developers who do not know containers a safe two-stage build,
but only for the languages Samuori ships. Anything else needs a Dockerfile, which developers must write and reviewers
must read line by line. Organizations need their own repeatable builds (for example a database base image with
tooling) that developers choose from the wizard and fill in, without writing or reviewing Dockerfiles.

## Decision

### What a custom recipe is
A versioned, declarative build definition an **administrator** authors in the portal as YAML (or JSON), validated
by Samuori when it is saved and previewed (`internal/recipedef`; unknown fields are refused). Samuori renders it into a Dockerfile itself; it is never free Dockerfile
text. A recipe has:

- **Identity:** `id` (lower-case, must not collide with a built-in), semantic `version`, display name,
  description, the languages and application types it suits, and its platforms.
- **Parameters** developers fill in the wizard: typed (`string` with pattern or enum, `version`, `port`, `path`),
  with defaults and a required flag. Values are validated against the parameter's type before rendering.
- **Stages**, in order; the last is the final image. Each has a name and a base: an image **pinned by digest**
  that passes the base-image policy and approved image sources, or an earlier stage.
- **Steps**, from a closed set. Each renders to exactly one Dockerfile instruction, in JSON exec form where there
  is a command, so no value can add instructions (the SEC-SPC-01 guarantee extends to recipes):

  | Step | Renders to | Notes |
  |---|---|---|
  | `workdir` | `WORKDIR` | path |
  | `copySource` | `COPY` from the upload | source and destination paths inside the context |
  | `copyFromStage` | `COPY --from=<stage>` | earlier stages only; optional owner |
  | `run` | `RUN` | argv or shell; `network: none` (default) or `egress`; `secrets:` logical IDs; optional user |
  | `env` | `ENV` | non-secret values only; names that look secret are refused |
  | `user` | `USER` | the final stage must end as a non-root user unless the recipe is approved with an exception |
  | `expose` | `EXPOSE` | port |
  | `label` | `LABEL` | |
  | `cmd` | `CMD` | argv |

  `${param}` may appear only in values (paths, ports, users, environment and label values, argv elements), never in
  step kinds, stage names, image references or option names. Every value is validated again after substitution.
  Parameters are **never pasted into shell scripts**: each is declared as a build argument in every stage, and a
  `shell` step reads it as `$name`; `${name}` in a shell step is refused.
- **Network:** `run` steps have no network unless marked `egress`, and then reach only the egress presets and
  destinations the recipe declares, through the existing egress proxy. Approving the recipe approves those.
- **Secrets:** the logical secret IDs its steps mount. Developers bind them with granted keys exactly as today; a
  recipe never names a store, path or value.
- Excluded: `ADD`, `VOLUME`, `SHELL`, `ONBUILD`, `HEALTHCHECK`, host mounts, `--privileged`, custom frontends,
  free-form build arguments and conditionals or loops.

### Lifecycle
- **Draft → approved → deprecated → withdrawn**, as built-in recipes are deprecated and withdrawn today. Only
  approved versions appear in the wizard; deprecated ones warn at admission, withdrawn ones are refused.
- An approved version never changes; editing creates a new draft version.
- **Approval** is by an administrator, with a reason, after a **preview** of the rendered Dockerfile and an
  optional **trial build** against an upload; both appear on the recipe page. Audited
  (`recipe.create`, `recipe.approve`, `recipe.deprecate`, `recipe.withdraw`).

### Developers
- Wizard step 4 lists approved custom recipes that match the application (or all, for "Describe it myself") beside
  the built-ins. Choosing one shows a form of its parameters.
- The spec records `build.method: recipe`, the recipe `id` and `version`, and `recipe.parameters`. It stays free of
  credentials and store paths (ADR 0029).
- The build plan carries the resolved recipe version and parameter values, so the worker renders the same
  Dockerfile the reviewer previewed, and the plan digest covers it.

### Unchanged
Validation, sensitive-output checks and output exceptions (ADR 0031), base-image and egress policy, worker
isolation, and publication gates apply to custom-recipe builds exactly as to any other build.

## Not in this decision
- A shared library of reusable steps across recipes (the rest of D16): a later version.
- Developers or maintainers authoring recipes: administrators only in this version.
- Rewriting the built-in recipes in the new format.

## Owner decisions (2026-10-21)
1. **Approval:** any administrator approves, with a reason.
2. **Non-root final stage:** required, unless the recipe version is approved with a recorded reason for running as
   root.
3. **Trial build before approval:** optional.

## Consequences
More capability without Dockerfiles, at the cost of a renderer and editor to maintain. Expressiveness is
deliberately limited; teams that need more keep using reviewed Dockerfiles.
