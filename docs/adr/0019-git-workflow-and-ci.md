# 0019. Git workflow, commits, pull requests and CI

Status: Accepted (2026-10-19). Commit identity, sign-off and attribution rules are in ADR 0022. The repository is
public; hosted CI is not set up yet.

## Decision
Details and commands are in [`../developers/git-workflow.md`](../developers/git-workflow.md).

- **Trunk-based:** `main` is always green and releasable; short-lived branches `<type>/<short-topic>`.
- **Conventional Commits** (`feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `perf:`, `build:`, `ci:`, `chore:`), scope =
  package or area. DCO sign-off and no AI attribution in Git metadata (ADR 0022).
- **Squash merge** with a Conventional Commit title; linear history.
- **PRs:** one focused change with tests, docs, migration notes, observability notes, traceability update and
  license impact. Template checklist in the workflow doc.
- **CI gates on every PR:** format check, lint, unit tests (Go + UI), generated-code drift, schema tests, migration
  equivalence, govulncheck, license policy check from a Syft SBOM, build of all binaries. Integration, E2E and security
  suites run on PRs touching their areas and nightly on `main`.
- CI is deterministic and bounded: pinned tools, no live cloud credentials, disposable registries/databases only.

## Consequences
CI definitions will live in `.github/workflows/`, none exists yet; `make ci` runs the same checks locally.
