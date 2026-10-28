# CLAUDE.md — Samuori

Samuori is an independent, self-hosted container image factory (prefix `samuori`). It unifies established open-source
build, scan and signing tools behind one guided, governed workflow. A developer
who knows their language but not containers uploads a project in the browser and gets a validated, governed OCI
image. Requirements and architecture live in `docs/` (index: `docs/README.md`); this file is the operating guide.

## Start of every session

1. Read the "Current position" section of `docs/roadmap.md` to find the current milestone and its open tasks.
2. Read only the docs relevant to the task. Check `docs/open-decisions.md` before choosing anything listed there.
3. **Execution mode:** work through all 28 work packages in
   `docs/roadmap.md` continuously: implement, integrate, fix, update status and traceability, commit, then
   continue. Progress reports are not approval checkpoints. Ask only about material changes to architecture,
   security, licensing, cost or scope, and keep doing unblocked work while waiting.

## Non-negotiable boundaries

- **Originality.** Write everything originally; copy no code, assets or text from other projects. Use public specs,
  original work and properly licensed OSS only. Private notes and any reference-reading rules live in the
  git-ignored `CLAUDE.local.md`; keep private or organization-specific information out of tracked files.
- **Focused scope.** Only discovery, configuration, build, test, security, storage, publication, promotion and their
  administration. Not Backstage, a developer portal, source hosting, a generic CI/workflow engine or deployment
  orchestration (`docs/product/vision-and-scope.md`).
- **Simple install first.** All-in-one needs no Kubernetes, Harbor, Vault/OpenBao, Keycloak, ECR, external DB or
  external message server. It does need Linux and BuildKit, with KVM for `vm-per-job` or a dedicated host
  (ADRs 0013, 0027).
- **Untrusted source.** Never execute uploaded code or package scripts during discovery. No host Docker socket,
  uncontrolled privileged execution, arbitrary host mounts or unrestricted secrets in builds
  (`docs/architecture/security.md`). Uploaded source builds only on pools with an approved **and verified** isolation
  profile (ADRs 0013, 0027). Windows execution is deferred, but keep every model platform-aware.
- **One build per BuildKit daemon**; a fresh daemon for every secret-bearing build, never reused; no cache reuse for
  secret-bearing builds until OD-19 (`docs/architecture/build-cache.md`). Enrollment never implies worker trust.
- **DB is the source of truth.** NATS is transport only; transactional outbox, fenced leases, idempotency keys.
- **Lifecycles stay separate:** BuildRun, ValidationRun and Publication (`docs/architecture/build-lifecycle.md`).
  Skipped or blocked is never success. Ready does not mean published.
- **Portable BuildSpec** holds no snapshot reference, credentials, account IDs or secret values/paths; secrets are
  logical IDs bound by admins per project (ADRs 0004, 0008, 0029). Existing Dockerfiles are first-class and analysed
  statically, never executed for discovery (ADR 0028). Secret mounts do not make outputs safe (ADR 0031). The API
  group is the placeholder `samuori.invalid/v1alpha1`.
- **Authorization is server-side** on every API call, dispatch, artifact access and publication. Hidden
  navigation is not authorization.
- **No fake success.** No mock success paths, placeholder endpoints or security boundaries, misleading dashboards,
  health without real checks, or production-readiness claims without negative and recovery evidence. Label
  unfinished features. Never fabricate test output. No broad speculative framework dependencies.
- **Licenses.** Record every new dependency with its verified license in `docs/dependencies.md`. Never bundle Vault
  (BUSL), Grafana or Loki (AGPL). GPL tools (golangci-lint, crun) are dev-only or exec-only.
- **Product decisions D01–D31** (`docs/product/decisions.md`) change only with owner approval via ADR. The entire
  product is one initiative (D31); milestones order the work and never reduce scope.

## Name and brand (ADR 0057)

- Product **Samuori**; CLI and binaries `samuori`, `samuori-server`, `samuori-worker`, `samuori-web`; environment
  prefix `SAMUORI_`; API group `samuori.invalid/v1alpha1`.
- Logo files are in `docs/brand/` and are used unchanged: flat variants in app chrome and below 64 px, 3D for large
  displays, the light or dark variant to match the surface. Never recolour, invert or redraw them.
- UI colours come from the semantic tokens in `ui/src/design/tokens.css`, never hard-coded values.

## Repository layout (ADR 0009)

```
README.md LICENSE NOTICE CONTRIBUTING.md SECURITY.md CODE_OF_CONDUCT.md Makefile go.mod   .github/workflows/
cmd/{samuori-server,samuori-worker,samuori-web,samuori}/   internal/<domain>/   internal/adapters/<kind>/<impl>/   internal/apiclient/
api/openapi/  api/schemas/         migrations/{sqlite,postgres}/   ui/  (generated TS: ui/src/api/generated/)
deploy/{compose,systemd,helm,dev}/   examples/   tools/   docs/
```

**YAML files use `.yml`, never `.yaml`** (mandatory). This covers config, CI workflows, Compose, Kubernetes
manifests, examples and fixtures, and tools must default to `.yml`. Never keep both names for one file. A `.yaml`
file is allowed only when an external tool or spec requires it, recorded in `docs/developers/toolchain.md`.

Focused regression tests are colocated `_test.go` files (ADR 0039 as amended). Everything Go is under `internal/`. Domain packages
never import vendor SDKs; adapters implement narrow domain interfaces.

## Toolchain (ADR 0011; details in `docs/developers/toolchain.md`)

Go toolchain go1.27.2 · Node 26.12.0 with npm · GNU make · OpenAPI 3.0.3 with oapi-codegen (Go) and
openapi-typescript + openapi-fetch (TS) · SQLite (modernc) / PostgreSQL 16–18 (pgx) · goose · embedded NATS ·
rootless BuildKit v0.34.0 · React Aria Components, Tailwind CSS 4, TanStack Query and Table, Lucide icons, under an original design system (ADR 0023, amended 2026-10-21). Provisional Go
module path: `samuori.invalid/samuori` (ADR 0008). On macOS, builds run in the Lima VM
(`deploy/dev/lima/`).

```
make generate | check-generated | fmt | fmt-check | vet | build | test | test-vm | release | api-check | yml-check | ui-check
make license-check | sbom | vuln | vuln-ui | ci | aio-clean | aio-compose | aio-debian | aio-amd64
```

## Working rules

- Docs change in the same change as behavior. Significant design choices get an ADR (`docs/adr/`). A conflict
  between docs is a bug to fix by ADR.
- Every change updates `docs/roadmap.md` task status and `docs/traceability.md` where affected, and states what is
  truly runnable and what remains.
- **Focused tests only** (ADR 0039 as amended): keep and add focused regression tests for security boundaries and
  practical workflow checks; do not rebuild the old suites or chase test counts. A control counts as verified only
  when a current test or a recorded practical run exercised it; earlier evidence in the docs is history. No cloud spend, live credentials, or employer/production
  accounts. Disposable resources only.
- Do not implement security controls whose design is still Proposed or pending an open decision.
- Both migration dialects change together. Generated code is committed and must not drift.
- Prototype risky integrations before depending on them; record evidence in `docs/spikes/`
  (`docs/architecture/integrations.md`).
- Open questions go to `docs/open-decisions.md` with a recommendation. Don't silently pick a conflicting
  interpretation.

## Git (ADR 0019; `docs/developers/git-workflow.md`)

Trunk-based, `main` always green, branches `<type>/<topic>`, squash merge, PR checklist.
Commit validated work locally without asking (owner directive). Push only at the
owner's direction. Never commit
`.private/`, `CLAUDE.local.md`, secrets or build outputs. Never use an employer email or account in Git identity.

## Commits and Pull Requests (ADR 0022)

**No AI attribution in Git history.** Commit messages, trailers, authors, committers and PR descriptions identify
the people responsible for the changes, not the AI tools that assisted them.

- No `Co-Authored-By:` trailers naming Claude, Copilot, ChatGPT or other AI assistants.
- No `Generated with Claude`, `Generated by AI`, `Assisted by` or similar signatures.
- No AI session links, promotional footers or tool-specific attribution.
- Never set an AI assistant as Git author or committer. Use the human contributor's configured Git identity.
- Concise, descriptive messages focused on the actual change; Conventional Commits where appropriate.
- PR descriptions explain changes, rationale, testing and relevant issues, without AI-generated commentary.
- Before committing, inspect the message and trailers for unwanted attribution. Before pushing, verify no
  attribution was inserted automatically. If it was, correct it before publication without rewriting unrelated or
  shared history.

**DCO.** The project uses the Developer Certificate of Origin, not a CLA (`CONTRIBUTING.md`). `Signed-off-by:`
trailers stay intact and identify the human contributor certifying the contribution. Add one only when that person
explicitly asks, using their identity. A DCO sign-off is not AI attribution.

These rules apply to every commit, pull request, automated workflow and contribution. AI-assisted development is
permitted, but AI tools are never represented as authors, committers or contributors in Git metadata. This does not
override disclosure, attribution, licensing or contribution obligations imposed by external projects or hosting
platforms.

## Code comments (ADR 0020)

**Code is implementation, not documentation.** Keep source code clean, concise and readable. Avoid excessive
comments, verbose descriptions, decorative section headers or lengthy explanations inside scripts, modules,
configuration files, Dockerfiles and YAML.

- **Prefer self-documenting code.** Use clear function names, variable names, types and logical structure instead
  of explaining obvious behavior through comments.
- **Keep comments short and purposeful.** Comment only when the reasoning, constraint, workaround or non-obvious
  behavior cannot be understood from the code itself.
- **Do not embed documentation in source files.** Architecture explanations, runbooks, security models,
  operational procedures and implementation histories belong under `docs/`.
- **Reference documentation when necessary.** Use a short comment pointing to the specific document or section
  rather than duplicating its content.
- **Avoid decorative comments.** No large ASCII banners, excessive separators or repetitive section headings.
- **Avoid redundant comments.** Do not explain what the next line of code obviously does.
- **Avoid commentary about development history.** No comments describing what Claude changed, why a previous
  implementation was replaced, or what was done during a coding session.
- **Avoid unnecessary docstrings.** Document public APIs, important contracts and complex behavior where
  appropriate; no lengthy descriptions for trivial functions.
- **Keep YAML and configuration comments minimal.** Configuration files contain configuration, not tutorials.
- **Keep security-critical comments precise.** State non-obvious security invariants, assumptions and restrictions
  briefly, with references to detailed documentation where appropriate.
- **Never sacrifice clarity to eliminate comments.** Complex algorithms, compatibility workarounds and subtle
  implementation decisions are still documented when necessary.

Avoid:

```go
// This function is responsible for checking whether the worker
// is currently available to receive additional build requests.
// It examines the number of running jobs and compares that value
// against the maximum concurrency configured by the administrator.
// If the worker has available capacity, the function returns true.
func canAcceptBuild(w Worker) bool {
    return w.ActiveBuilds < w.MaxBuilds
}
```

Prefer:

```go
func canAcceptBuild(w Worker) bool {
    return w.ActiveBuilds < w.MaxBuilds
}
```

For complex behavior:

```go
// Lease fencing prevents stale workers from completing reassigned jobs.
// See docs/architecture/build-lifecycle.md#worker-leases.
```

Applies to Go, TypeScript, React, shell, Dockerfiles, YAML, SQL and all other implementation files. In reviews and
refactoring, remove redundant commentary and keep comments that explain contracts, security constraints or
non-obvious behavior.
