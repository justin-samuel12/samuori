# 0051. Organization runtimes for built-in recipes

Status: **Accepted** (2026-10-26, owner decision: Option A). Extends D16 and [ADR 0041](0041-custom-recipes.md).

## Context
Built-in recipes (`internal/recipes/catalog/*.yml`) ship with Samuori and are read-only. Each lists its runtimes, for
example Node.js 22 and 24, with a build image and a run image pinned by digest. Offering a new runtime version, such
as a new Node.js major, needed a Samuori release. Organizations want to offer one as soon as its upstream image exists,
without waiting for a release and without writing a custom recipe.

## Options
- **A. Administrators add runtime versions to built-in recipes in the portal (chosen).** Shipped runtimes stay
  read-only; additions are stored per organization and layered on top, so a Samuori update never overwrites them.
- **B. Copy a built-in recipe into a custom recipe and edit it (rejected).** The copy loses the built-in's defaults,
  wizard support and future fixes, and every organization would maintain a fork.
- **C. Edit built-in recipes in place (rejected).** Edits would be overwritten by the next Samuori release, or would
  block it, and shipped definitions would no longer be the same in every installation.

## Decision
- An **organization runtime** belongs to one organization and one built-in recipe version. It has a version
  **label** (`24`, `3.14`, `10.0`: one to three numeric parts, like shipped labels), a **build image** and a **run
  image**, and an optional note. It does not change the recipe's stages, so the rendered Dockerfile is the same as
  for a shipped runtime with those images.
- Both images must be full references **pinned by digest** and allowed by the organization's **base-image policy**
  when the runtime is added and again when it is approved. Admission checks them against the policy for every build,
  as it does for shipped runtimes.
- A label may not repeat a shipped runtime of that recipe version or an active (draft or approved) organization
  runtime. Withdrawn runtimes stay as history, and their label may be added again.
- Lifecycle **draft → approved → withdrawn**; a draft may also be withdrawn. Only organization administrators add,
  approve and withdraw, each with an audit event; approval and withdrawal need a reason. As for custom recipes, any
  organization administrator may approve, including the one who added it; there is no second-approver rule.
- **Resolution.** Shipped runtimes are preferred: a version constraint resolves to the newest matching shipped
  runtime, and only when none matches to the newest matching approved organization runtime. Adding a runtime
  therefore never changes what an existing spec resolves to. If a later Samuori release ships the same label, the
  shipped runtime wins and the organization runtime is shown as superseded. A constraint that matches only a draft
  or withdrawn organization runtime is refused at admission and at planning with that reason.
- The **plan** records the runtime label and both images by digest, and for an organization runtime its ID and
  source (`runtimeId`, `runtimeSource`), so the plan digest covers them. Plans for shipped runtimes are unchanged.
- **API.** `GET /recipes` lists offered runtimes (shipped and approved organization ones) with `source` and
  `status`; drafts and withdrawn runtimes never appear there, so the wizard needs no filtering.
  `GET|POST /recipes/{id}/versions/{version}/runtimes` and `POST …/runtimes/{runtimeId}/actions` manage them;
  reading follows `/recipes` (non-administrators see approved runtimes only).

## Consequences
- New runtime versions no longer wait for a Samuori release. The organization owns the choice of image and digest, and
  the base-image policy still bounds which repositories are allowed.
- An organization runtime uses one run image for every application type; a recipe whose shipped runtimes use a
  different run image per type (`dotnet-service`) gets the single run image given.
- Organization runtimes are not added to the default base-image policy; a repository outside it must be allowed
  first (for example through a base-image request).
