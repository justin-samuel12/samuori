# Git Workflow, Pull Requests and CI

Status: **Authoritative** (ADR 0019).

> Pushes to the public repository are the owner's decision. The permanent Go module path is OD-2b in
> `docs/open-decisions.md`.

## Branches

- `main` always passes `make ci`.
- Work branches: `<type>/<short-topic>`, e.g. `feat/upload-chunking`, `fix/lease-fencing`, `docs/adr-0021`.
- Keep branches short-lived; rebase on `main` rather than merging `main` in.

## Commits

Conventional Commits: `<type>(<scope>): <imperative summary>`. Types: `feat`, `fix`, `docs`, `test`, `refactor`,
`perf`, `build`, `ci`, `chore`. Scope is a package or area (`scheduler`, `ui/wizard`, `adr`).

- One logical change per commit; the body explains *why* when it is not obvious.
- DCO sign-off (`git commit -s`) from the human contributor; see `CONTRIBUTING.md` and ADR 0022.
- No AI attribution in any Git metadata: no AI co-author trailers, "generated with" footers, session links, or AI
  author/committer identities. Use the human contributor's identity, never an employer email.
- Never commit secrets, `.private/`, `CLAUDE.local.md`, `.dev/`, `bin/` or `dist/`.

## History rewrites

The public repository starts from a fresh history (ADR 0057). Shared history is never rewritten.

## Pull requests

Squash-merged with a Conventional Commit title. Template checklist:

- [ ] Focused change; linked roadmap task and decision IDs (D01–D31)
- [ ] Focused regression test for any change to a security boundary or practical workflow (ADR 0039 as amended); otherwise say how it was checked
- [ ] Docs updated (product/architecture/operator) in the same PR; ADR added for significant decisions
- [ ] `docs/traceability.md` and `docs/roadmap.md` status updated
- [ ] Migrations for both dialects, with upgrade/rollback notes
- [ ] Observability: logs/metrics/audit events for new behavior
- [ ] License impact: new dependencies recorded in `docs/dependencies.md`
- [ ] Generated code regenerated (`make check-generated` passes)
- [ ] Comments follow the code comment standard (no redundant or history comments)
- [ ] YAML files use `.yml` (exceptions only as listed in `toolchain.md`)
- [ ] Original work: nothing copied from other projects or from material you have no right to contribute
- [ ] Every commit has a human DCO sign-off and no AI attribution in messages, trailers, author or committer

## CI

Every PR: `make ci` and `make vuln`. Hosted CI is not set up yet, so contributors run them locally; `make test-vm`
runs the Linux-only tests on macOS. `make ci` passing is not full coverage (ADR 0039 as amended).

Rules: pinned tool versions; no live cloud credentials or employer accounts; disposable resources only.

## Migrations

Both dialects change together. Within a minor release, migrations are additive (new tables, nullable or defaulted
columns, new indexes), so controllers of the previous minor release keep working during a rolling upgrade (ADR 0036).
Dropping, renaming or narrowing anything waits for the next minor release.
