# 0003. Separate BuildRun, ValidationRun and Publication lifecycles

Status: Accepted (2026-10-19) — owner decision

## Context
The original directive defined two incompatible build state machines (one with `preparing`, one with
`pending_validation`/`security_check`/`blocked`/`expired`) and simultaneously required that publication be modeled
separately so `ready` does not imply `published`. Validation and security checks must also be re-runnable against an
existing digest (rescan, promotion revalidation), which a build-scoped state cannot express.

## Decision
Three lifecycle models, specified in [`../architecture/build-lifecycle.md`](../architecture/build-lifecycle.md):

1. **BuildRun** (with **BuildAttempt** records) produces an ImageArtifact identified by digest.
2. **ValidationRun** targets an image digest; contains CheckResults (tests, scans, SBOM, provenance, signature) with
   outcomes `passed | failed | blocked | skipped | errored` and a derived verdict.
3. **Publication** targets a digest and destination; `kind = publish | promote`. Promotion is a Publication with a
   source publication, never a rebuild.

**Readiness** is not a lifecycle or state: it is derived at publish time from policy, the latest ValidationRun and
valid exceptions.

Original states are all retained and mapped (table in the lifecycle document); `pending_validation` is renamed
`pending_admission` because it described spec/policy admission, not image validation.

## Consequences
- UI shows one timeline with three independent outcomes.
- `publication_jobs` and `promotions` tables collapse into `publications`; `validation_runs` and `check_results`
  are added.
- Re-validation and promotion gates reuse one code path.
