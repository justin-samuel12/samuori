# 0002. Documentation structure and authority

Status: Accepted (2026-10-19)

## Context
The original `CLAUDE.md` (≈47 KB) combined operating instructions with the full product requirements. It was loaded
into every session, buried the critical rules, and contained conflicting definitions (build states, snapshot
placement, editor mode names, wizard steps, admin sections).

## Decision
- `CLAUDE.md` is a short operating guide: boundaries, commands, layout, conventions, current milestone, pointers.
- Requirements and architecture live under `docs/`, one authoritative location per topic (`docs/README.md` is the
  index). Conflicts are resolved by ADR, not duplicated.
- Authority order when documents disagree: owner decisions in `docs/product/decisions.md` → accepted ADRs →
  `docs/product/*` and `docs/architecture/*` → `CLAUDE.md` summaries. A discovered conflict is a bug: fix it with an
  ADR in the same PR.
- The verbatim original directive is archived privately at `.private/original-CLAUDE-2026-10-18.md` (git-ignored). Every requirement in it is carried into `docs/`; contradictions are resolved in
  ADRs 0003–0007. The coding comment standard added afterwards is ADR 0020.

## Consequences
Sessions read only the documents relevant to the current task. Docs are updated in the same PR as behavior.
