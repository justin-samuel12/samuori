# 0042. No legal-status tracking; private information handling

Status: Accepted (2026-10-21) — owner decision. Replaces ADR 0018.

## Context
ADR 0018 kept a legal-status record that gated implementation, hosting, publication and the use of cloud accounts.
The owner decided to stop tracking legal status in the repository.

## Decision
- The repository holds no legal-status record and no gates on it. What runs where, and what is published, is the
  owner's decision.
- These engineering rules stay:
  - Private notes stay in git-ignored files (`CLAUDE.local.md`, `.private/`). Tracked files name no organization
    other than upstream projects Samuori credits.
  - Samuori is written originally: no code, assets or text are copied from other projects.
  - Dependencies keep their recorded, checked licenses (`docs/dependencies.md`, `make license-check`).

## Consequences
Hosting, publication and evaluation environments are owner decisions recorded in the roadmap or the relevant
operator guide, not gates.
