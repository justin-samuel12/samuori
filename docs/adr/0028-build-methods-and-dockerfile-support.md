# 0028. Build methods: generated recipes and first-class Dockerfiles

Status: **Accepted as direction** (2026-10-19, owner decision OD-13). Design:
[`../architecture/build-execution.md`](../architecture/build-execution.md),
[`../architecture/buildspec.md`](../architecture/buildspec.md).

## Context
Samuori must make simple builds easy for developers with no container knowledge. It must also accept production-grade,
multi-stage Dockerfiles (secrets, certificates, database initialization) without forcing experienced users to
rewrite them into a Samuori-specific format.

## Decision
- A BuildSpec selects one **build method**:
  - `recipe`: Samuori generates the build definition from an approved, versioned recipe.
  - `dockerfile`: the user's Dockerfile, in the source snapshot, is the build definition.
- The BuildSpec stays declarative. It references the Dockerfile and states intent: context, target stage, non-secret
  build arguments, logical secrets, platforms, validation and outputs. It never re-expresses Dockerfile
  instructions.
- Supported Dockerfile features include:
  - syntax directives, from approved frontends only;
  - `ARG`, `ENV` and multiple stages;
  - BuildKit heredocs;
  - `RUN --mount` of types `secret` and `cache`, plus `bind` from the build context or another stage;
  - `COPY` / `ADD` from the context and named contexts;
  - custom scripts and entrypoints;
  - target stages and multi-platform builds where the engine supports them.
- Dockerfiles are analysed **statically**, with BuildKit's Dockerfile parser used as a library. Analysis covers
  stages, args, secret mounts (ID, target, required), contexts and frontend directives. Nothing is executed and no
  frontend image is pulled during analysis. Anything that cannot be determined statically is reported as ambiguous
  and needs user confirmation.
- `# syntax=` directives may name only frontends on an admin-approved, digest-pinned list. Any other frontend is
  refused before scheduling, because custom frontends run arbitrary code inside the build engine.
- `RUN --mount=type=ssh` and `ADD` from remote URLs are detected and refused with an explanation until separately
  designed. Remote `ADD` bypasses the egress design.
- Generated recipes produce a Dockerfile internally. It is shown read-only for review, stored as a build artifact,
  and contains no secret values.
- Samuori never silently rewrites developer-provided Dockerfile instructions, and the product is not limited to what
  generated recipes can express.

## Consequences
- Advanced users bring Dockerfiles unchanged. Beginners never see one unless they ask.
- The Guided view of a `dockerfile`-method spec shows intent fields only. Instruction-level editing happens in the
  user's own Dockerfile, not in Samuori.
- A BuildKit library dependency enters the controller's discovery code; it is recorded in `docs/dependencies.md`.
