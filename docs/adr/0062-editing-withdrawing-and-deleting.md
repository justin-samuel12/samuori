# 0062. Renaming, withdrawing requests and deleting pipelines and conversions

Status: **Accepted** (2026-10-28, owner request to make the UI editable wherever it is safe; provisional, may change
during acceptance testing). Extends [ADR 0058](0058-archiving-restoring-and-purging-projects.md),
[ADR 0033](0033-git-pipelines-and-webhooks.md) and [ADR 0048](0048-kubernetes-artifact-generation.md).

## Context
The owner asked for add, edit and remove everywhere it is safe. Projects, pipelines and Kubernetes conversions could
be created but not renamed or removed, and a requester could not take back a request that was no longer needed.
Decided approvals are immutable audit objects (`../architecture/security.md#authorization`), records keep history,
and a pipeline's runs point at it.

## Options
- **Withdrawal as a closing state of its own (chosen):** a pending request becomes `withdrawn`, recorded with who and
  why; it is not a decision and cannot change one.
- **Reuse `expired` or `rejected` for withdrawal (rejected):** history would say an administrator rejected it or time
  ran out, which is false.
- **Delete pending requests (rejected):** loses the record that someone asked.
- **Hard-delete pipelines (rejected):** their runs reference them and link to the builds they started.
- **Soft-delete conversions (rejected):** a conversion's revisions are generated files the person asked to remove;
  the audit trail already keeps what happened.

## Decision
- **Project rename.** `PATCH /projects/{id}` accepts `name` with the creation rule and per-team uniqueness (`409` on a
  clash), for maintainers and administrators of the project's team, audited as `project.update` with the old and new
  name. The request schema has no team field: a project never moves to another team. Archived projects are not
  renamed.
- **Withdrawal.** `POST /approvals/{id}/withdrawal` (optional reason, at most 2048 characters) withdraws a pending
  repository, base-image, egress-access or secret-binding request. The requester (while still holding a developer,
  maintainer or administrator role on the project) or a maintainer or administrator of the project may withdraw it;
  others get `403`, other teams `404`. It is a compare-and-swap from `pending`, so a decided, expired or already
  withdrawn request gets `409` and decisions stay immutable. The subject's closing step runs in the same
  transaction, so a withdrawn base-image or egress request no longer blocks a new one. Publications keep their own
  lifecycle and cannot be withdrawn this way. Audited as `approval.withdraw`. Approvals and listed requests carry
  `canWithdraw` for the UI. Migration 00070 adds `withdrawn` to the approval states.
- **Pipelines.** `PATCH /pipelines/{id}` edits name, repository URL, refs, pinned spec revision, username and the
  repository token (write-only; a new controller-only secret, never returned). The webhook URL, scheme and secret,
  the owner, project and spec do not change. `DELETE /pipelines/{id}` is refused while a run is queued or fetching;
  otherwise it disables the pipeline, sets `deleted_at` and `deleted_by` (migration 00070), frees its name and drops
  its references to its secrets, which become unused controller-only secrets an administrator may delete. Runs and
  the builds they started are kept; the webhook answers as for an unknown pipeline. Administrators only, as for
  create; both audited.
- **Kubernetes conversions.** `PATCH /kube-conversions/{id}` renames (developer, maintainer, administrator, as for
  create); `DELETE /kube-conversions/{id}` removes the conversion, its runs, revisions and files, for its creator or a
  maintainer or administrator of the project, refused while a run is queued, assigned or running. File edits stay new
  revisions. Both audited; both refused on an archived project.

## Consequences
- History and audit stay complete: withdrawn requests remain visible as withdrawn, deleted pipelines keep their runs.
- A deleted pipeline's secrets need a separate delete under Secrets, as after a token replacement or a webhook-secret
  rotation.
- Not verified: PostgreSQL runs of migration 00070 and of these paths, and the UI in a browser.
