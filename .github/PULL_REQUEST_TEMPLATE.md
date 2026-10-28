## What and why

<!-- The change, the reason for it, and related issues or decisions (D01–D31, ADRs). -->

## How it was tested

<!-- Commands run and their results, and any manual checks. -->

## Checklist

See `docs/developers/git-workflow.md`.

- [ ] One focused change; `make ci` passes locally
- [ ] Focused regression test for a change to a security boundary or practical workflow, or a note on how it was checked
- [ ] Docs updated in the same change; ADR added for a significant decision
- [ ] `docs/roadmap.md` and `docs/traceability.md` updated where affected
- [ ] Migrations for both SQLite and PostgreSQL, with upgrade notes
- [ ] New dependencies and their licenses recorded in `docs/dependencies.md`
- [ ] Generated code regenerated (`make check-generated`)
- [ ] Comments follow ADR 0020; YAML files use `.yml`
- [ ] Original work, with nothing copied from material you have no right to contribute
- [ ] Every commit has a human DCO sign-off (`git commit -s`) and no AI attribution
