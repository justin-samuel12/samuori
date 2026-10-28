# 0060. Deprecating and withdrawing built-in runtimes

Status: **Accepted** (2026-10-28, owner decision; provisional, may change during acceptance testing). Extends
[ADR 0051](0051-organization-runtimes.md) and the recipe status rules in
[recipes and policies](../architecture/recipes-and-policies.md).

## Context
An organization administrator could deprecate or withdraw a whole built-in recipe version, and withdraw a runtime the
organization added, but not one runtime that Samuori ships. Retiring Node.js 22 while keeping Node.js 24 in
`node-service` 1.0.0 meant withdrawing the whole recipe version or nothing.

## Options
- **Granularity:** a status per shipped runtime, keyed by recipe ID, recipe version and runtime label (chosen: the
  label is how specs and plans already name a shipped runtime); a status per runtime label across all recipe versions
  (rejected: recipe versions pin different images and are retired separately).
- **Storage:** a row per decision, none for approved (chosen: Samuori updates never touch it and the default needs no
  seeding); a column on the catalog (rejected: the catalog is embedded and shared by every organization).
- **Same-label organization runtime when the shipped one is withdrawn:** stays shadowed (chosen); becomes eligible
  (rejected: withdrawing a runtime would silently swap its images for ones that were approved for a different purpose,
  possibly long before, and a spec asking for `22` would keep building without anyone deciding that).
- **When it applies:** at admission and planning (chosen, as for recipe status); also to builds already planned or
  queued (rejected: the plan is immutable and digest-pinned, and changing it after admission would break the
  BuildRun lifecycle).

## Decision
- **Status.** Each shipped runtime is `approved` (no row), `deprecated` or `withdrawn` per organization, with a
  required reason, the administrator and the time (migration 00068, `shipped_runtime_status`). Approving again deletes
  the row; the audit log (`recipe.runtime.status`) keeps every change.
- **API.** `PUT /recipes/{recipeId}/versions/{version}/shipped-runtimes/{label}/status` with `{status, reason}`;
  organization administrators only (`x-samuori-roles: [administrator]` and an organization-wide binding, checked on
  the server), CSRF-protected. `GET /recipes` and `GET .../runtimes` report each shipped runtime's status and reason;
  only administrators see withdrawn runtimes and who decided.
- **Resolution.** A version constraint resolves to the newest matching non-withdrawn shipped runtime, then to the
  newest matching approved organization runtime whose label no shipped runtime uses, withdrawn or not. `>=22` picks 24
  when 22 is withdrawn. A constraint that matches only withdrawn runtimes is refused at admission
  (`runtime-unavailable`: "runtime 22 of Node.js application has been withdrawn by an administrator: <reason>") and
  at planning.
- **Deprecated** runtimes resolve and build as before; admission adds a `runtime-deprecated` warning with the reason,
  also for a frontend's Node.js runtime.
- **Builds already planned or queued are unaffected.** Recipe status changes have no impact preview, so this has none.
- **UI.** The Runtimes dialog under **Recipes & base images** shows each shipped runtime's status with **Deprecate**,
  **Withdraw** and **Approve again**, each asking for a reason. New image hides withdrawn runtimes and marks
  deprecated ones.

## Consequences
- An organization can retire one runtime without retiring the recipe version; other organizations are unaffected.
- A spec pinned to an exact withdrawn label stops building until it asks for another runtime or the runtime is
  approved again.
- The decision is evidence in the audit log, not in the plan: a plan records the runtime it resolved, not the status
  it had.
