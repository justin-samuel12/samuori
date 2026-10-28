# 0004. Source snapshot is a BuildRun input, not part of BuildSpec

Status: Accepted (2026-10-19) — owner decision

## Context
The original directive listed a "source snapshot reference" among BuildSpec fields in one section and said the
snapshot ID belongs to BuildRun input, not to a transferable reusable spec, in another.

## Decision
The BuildSpec is portable and reusable across sources. `source_snapshot_id` and its content digest are BuildRun
inputs. The resolved, immutable build plan (ADR 0014) binds snapshot digest and spec digest together for execution.

## Consequences
- The JSON Schema has no snapshot field; tests assert a spec validates identically against any snapshot.
- Re-running a spec on a new upload creates a new BuildRun, not a new spec revision.
- Detection evidence stored in the spec is informational only and may be refreshed by a new discovery.
