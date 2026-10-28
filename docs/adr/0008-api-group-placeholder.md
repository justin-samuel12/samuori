# 0008. API group placeholder until a controlled domain exists

Status: Accepted (2026-10-19). Final domain: **open** (see `docs/open-decisions.md`). Names as in ADR 0057.

## Context
An API group should be a DNS name the project controls. The project controls no domain yet, and renaming
`apiVersion` after release is costly.

## Decision
Use `samuori.invalid/v1alpha1` until the owner selects a domain the project controls. `.invalid` is reserved by RFC 2606
and cannot be registered by anyone, so it can never collide with a real owner and is obviously provisional.

- The group string is a single constant in Go and in the JSON Schema `$id` base.
- A release-gate check must fail if any `.invalid` group or schema ID remains (not implemented yet).
- Before the first public release the owner supplies the domain; a migration rewrites stored revisions.

### Go module path (OD-2, 2026-10-19)
The Go module path is the provisional `samuori.invalid/samuori`. It is syntactically valid, cannot
belong to anyone, and is associated with no employer account or domain. The repository is published, but the module
path stays provisional until the owner chooses its permanent location (OD-2b); it then changes in one commit
(`go.mod` plus imports), and the same release-gate check covers it.

## Consequences
Internal and spike work proceeds without blocking on naming. Changing the constant is a one-line code change plus
a stored-revision migration.
