# Working in Samuori

Concise guidance for anyone, human or AI, changing Samuori. The rules below protect real guarantees; breaking one
breaks a promise Samuori makes to its users. Detailed documentation is in [`docs/`](docs/README.md), and AI
assistants also follow [`CLAUDE.md`](CLAUDE.md).

## What this is

A self-hosted container image factory: a developer uploads a project, and Samuori discovers how to build it, builds
it in isolation, validates the image and publishes it, with every step governed and audited. It brings established
open-source build, scan and signing tools together behind one guided workflow.

It generates starting deployment files, but it **deploys nothing**: it is not a CI system, a workflow engine, a
developer portal, a source host or an orchestrator (`docs/product/vision-and-scope.md`).

## Invariants

- **Uploaded code is untrusted.** Discovery reads it and never runs it. Builds run only on pools with an approved,
  verified isolation profile: one build per BuildKit daemon, a fresh daemon for any build with secrets.
- **The database is the source of truth.** NATS only carries messages; state changes go through the transactional
  outbox.
- **Skipped or blocked is never success.** Build, validation and publication are separate lifecycles, and ready does
  not mean published.
- **Authorization is checked on the server for every call.** Hiding a button is never the protection; edit and
  delete need the same or stricter roles than create.
- **What is recorded stays recorded.** Snapshots, spec revisions, decided approvals, audit records, published tags
  and approved recipe versions never change. A change is a new version or revision.
- **Images are pinned by digest.** No moving tag is trusted at build or publication time.
- **No fake success.** No placeholder endpoints, health that checks nothing, or readiness claims without negative
  and recovery evidence. Label unfinished work.
- **`.yml`, never `.yaml`**, except where an external tool requires the name (`docs/developers/toolchain.md`).

## Names and publication

- The product is **Samuori**: binaries `samuori*`, environment prefix `SAMUORI_`, API group
  `samuori.invalid/v1alpha1` (ADR 0057). Use only these names.
- No employer, customer or other organization names anywhere in tracked files, commits or examples. Use neutral
  examples: `web`, `api`, `worker`, `registry.example.com`.
- Features are **generic**. Never shape code, docs or tests around one particular application.
- Private notes stay in git-ignored files (`CLAUDE.local.md`, `.private/`), never in tracked ones.

## Documentation

- `docs/` is authoritative and changes in the same change as behaviour. Significant design choices get an ADR in
  `docs/adr/`; every change updates `docs/roadmap.md` and `docs/traceability.md` where it applies.
- Markdown outside `docs/` is limited to the root files (`README.md`, `CONTRIBUTING.md`, `SECURITY.md`,
  `CODE_OF_CONDUCT.md`, `CLAUDE.md`, `SKILLS.md`) and GitHub's templates under `.github/`.

## Before committing

```sh
make generate    # after any OpenAPI or proto change; generated code is committed
make ci          # must exit 0
```

Both migration dialects (`migrations/sqlite`, `migrations/postgres`) change together, in order.

## Git

- Trunk-based on `main`; topic branches are `<type>/<topic>`. Work on the branch the change belongs to; don't
  create one nobody asked for.
- Push only when the owner asks. Never force-push shared history.
- Commits carry a DCO `Signed-off-by:` for the person responsible (`git commit -s`).
- **No AI attribution in git history:** no `Co-Authored-By:` for an assistant, no "Generated with …" footers, no
  session links, and no assistant as author or committer. Remove anything an assistant added before pushing.

## Working in parallel without overheating the machine

Do independent work in parallel: separate agents in their own worktrees, and background commands. But never overload
or overheat the machine:

- At most **two heavy jobs at once** across every agent and session. Heavy means `make ci`, race-detector tests,
  release or Docker/VM builds, browsers and dev servers.
- At most **one `make ci` at a time**, and at most **five agents** running at once.
- Before heavy work, check the load (`sysctl -n vm.loadavg`) and throttling (`pmset -g therm`); wait if the machine
  is busy. Run heavy jobs with `nice -n 10`, each with its own log file.
- Stop servers, browsers, VMs and worktrees when done. Say so when work is held back, so slow isn't mistaken for
  stuck.

## The UI is developer friendly

- Everything a person can add, they can also **edit and remove or delete**, with the same or stricter roles.
- Where something is immutable by design, the UI says so in one line and offers the safe equivalent: "Edit → saves as
  a new version", "Duplicate", "Withdraw", "Retire" or "Disable".
- Refusals explain what blocks them, for example "in use by these pools", and what to do instead.
- After an action, the person lands somewhere sensible with a short confirmation; they never have to reload or
  navigate back to see the result.
- Colours come from the semantic tokens and logos from `docs/brand/`, unchanged.
- A change that adds or changes a screen updates its scene in `ui/e2e/tour/scenes.ts`, runs `make screenshots` (the
  README's images) and `make screenshots-wiki` (the full gallery); `make screenshots-check` (in `make ci`) fails on a
  route without a scene.

## Code and comments

- **Code is implementation, not documentation** (ADR 0020). Comments explain *why*: a non-obvious constraint, a
  security invariant or a trap someone would reintroduce. Anything longer belongs in `docs/`, with a short comment
  pointing to the exact section: `// See docs/architecture/secrets.md#grants.`
- Shell scripts use `set -eu`, quote every variable, use `mktemp` and `trap` for temporary files, and never `eval`.

## Size and judgment

Solve the actual problem with the smallest change that is safe, clear and maintainable.

- Prefer existing patterns to new abstractions, direct code to frameworks, and one clear path to configurable ones.
- Don't build for hypothetical requirements, add helpers for one-off logic, or refactor unrelated code while fixing
  something narrow.
- Before adding a file, function, abstraction or option, ask what concrete problem it solves.

## Testing

Tests are focused and proportional (ADR 0039): each one protects a security boundary or a real workflow.

- Test the changed behaviour and the boundary it touches (roles, other teams, secrets not leaking, audit, refusals),
  not every permutation.
- Reuse the existing test helpers; keep fixtures and setup minimal.
- Keep the suite fast. Fix slow tests by making setup cheaper, never by adding parallel jobs to hide the growth.
- Keep `-race` in `make test`; it finds bugs nothing else does.
- Tests for one package stay with that package (colocated `_test.go` files).
- Never claim a check passed if it didn't run. Report what was not verified, for example "not run on PostgreSQL" or
  "not checked in a browser".

## Do not

- Run uploaded code during discovery, or weaken the isolation, egress or secret rules.
- Turn Samuori into a deployer, orchestrator or general CI system.
- Mutate a recorded version instead of creating a new one.
- Add a feature without its edit and remove path, or without docs.
- Put employer, customer or former product names into tracked files.
- Add AI attribution to commits or pull requests.
- Start heavy jobs without checking the load first.
