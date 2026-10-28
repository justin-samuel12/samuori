# 0001. Record architecture decisions

Status: Accepted (2026-10-19)

## Context
The initiative is large (31 product decisions, many integrations) and will be worked on across many sessions by
humans and Claude Code. Unrecorded decisions get re-litigated or silently changed.

## Decision
Record significant engineering decisions as ADRs in `docs/adr/`, numbered sequentially, using the format in
`docs/adr/README.md`. Open ADRs for unresolved engineering details instead of silently picking an interpretation.
Product decisions (D01–D31) are owner decisions; an ADR may clarify how one is realized but may change one only with
recorded owner approval.

## Consequences
Every PR that makes a significant design choice adds or updates an ADR. Superseded ADRs are kept, not deleted.
