# 0010. OpenAPI toolchain and generated client locations

Status: Accepted (2026-10-19). Tool versions and licenses are recorded in `docs/dependencies.md`.

## Context
The API is the single path for UI, CLI and pipelines (D29). Clients must be generated, not hand-written, and the
generator must be stable for both Go and TypeScript.

## Decision
- **Spec-first.** `api/openapi/samuori.v1.yml` is the source of truth, authored as **OpenAPI 3.0.3**. oapi-codegen added
  only *initial* 3.1 support in v2.8.0 (2026-07; multi-type unions become `any`), so 3.0.3 avoids weakly typed
  generated code. Revisit with a new ADR once 3.1 support matures.
- BuildSpec and other document schemas are standalone **JSON Schema 2020-12** files in `api/schemas/`, referenced by
  the API and validated in Go at runtime.
- **Go server and client:** `oapi-codegen` generates a `net/http` server interface into `internal/api/apigen/` and a
  client into `internal/apiclient/`. `tools/genunimpl` emits an `Unimplemented` type that answers **501** with a typed
  Problem for every operation; `internal/api` embeds it and overrides operations as they are built, so unbuilt
  endpoints are honest rather than fake. Generated files are `*.gen.go`.
- **TypeScript client:** `openapi-typescript` generates types to `ui/src/api/generated/schema.d.ts`; `openapi-fetch`
  is the runtime client wrapped in `ui/src/api/client.ts`.
- Generated code is **committed**. `make generate` regenerates; CI runs `make check-generated` and fails on drift.
- Generators run as pinned Go tool dependencies (`go tool`) and pinned npm devDependencies; no global installs.

## Consequences
The UI builds without a Go toolchain. API changes appear in review as contract diff plus generated diff.
