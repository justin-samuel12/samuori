# 0020. Code comment standard

Status: Accepted (2026-10-19) — owner decision

## Decision
Code is implementation, not documentation. The full rule set and examples are in `CLAUDE.md` ("Code comments") and
apply to Go, TypeScript/React, shell, Dockerfiles, YAML, SQL and all other implementation files. Explanations
belong in `docs/`; source comments state only non-obvious reasoning, contracts, security invariants or workarounds,
with a short reference to the relevant document where useful. No comments about development history.

## Consequences
Reviews remove redundant commentary. The PR checklist includes this rule. No linter enforces it automatically;
golangci-lint's comment linters are configured only for exported-API doc comments.
