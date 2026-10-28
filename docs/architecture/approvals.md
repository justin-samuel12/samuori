# Approvals

Status: **Implemented** for publications and secret-binding requests (WP22), security exceptions (WP25),
repository requests (WP27), base-image requests and egress access requests (ADR 0052).

An approval is a request someone other than the requester must decide. Each approval has:

- the project and the subject (`publication`, `secret-binding`, `security-exception`, `repository-request`,
  `base-image-request`, `egress-access-request`);
- a summary;
- the requester;
- an open-until time (`expiresAt`);
- once decided: the decider, the reason (at least 10 characters), when it was decided, and an optional
  `validUntil`.

| Rule | How it holds |
|---|---|
| Decided once | A compare-and-swap from `pending`; later decisions are refused (`409`) |
| Never by the requester | Separation of duties; the UI shows `canDecide` and the API refuses with `own_request` |
| Right deciders | Publications: publisher, security reviewer or administrator covering the project. Secret bindings and repository requests: administrators. Base-image and egress access requests: organization-scoped administrators only, since they change an organization policy or widen a project beyond it |
| Visibility | Deciders and anyone who can read the project; other teams never see the request |
| Expiry | Pending requests lapse at `expiresAt` (publications 72 hours, binding and repository requests 7 days, base-image and egress access requests 14 days); the reconciler marks them `expired` |
| Audit | Every request and decision is an audit event |

## Publications

A destination with **Publications need an approval** holds new publications in `awaiting_approval` with a
pending approval. Approval moves the publication to `approved` and queues it. Rejection or expiry ends it as
`rejected` or `expired`. The publish-time gate checks the approval again: it must be approved and still within
`validUntil`.

## Secret-binding requests

A developer who needs a secret for a build asks for it from the wizard's secrets panel (**Ask for binding**) or with
`POST /projects/{id}/secret-requests`. The request names the logical ID and why the build needs it. An administrator
binds a secret under **Secrets**, then approves the request, or rejects it. Developers never see or enter values.

## Repository requests

A developer or maintainer who needs a new publication repository asks for one on the project page (**Request a
repository**) or with `POST /projects/{id}/repository-requests`. The request names a lower-case repository path,
optionally a registry, and why it is needed (D03). Administrators decide it:

- **Fulfill:** create the repository (for ECR, under Registries with an administrator profile; IAM stays
  authoritative) and a destination open to the project, then approve with
  `POST /approvals/{id}/fulfillment` and that destination. The destination must be open to the request's project,
  and in the requested registry when one was named. Approving without a destination is refused (`unfulfilled`).
- **Reject** with a reason, through the normal decision.

Repository creation stays a separate administrator action, so no network call runs inside the approval transaction.
Developers see their project's requests with their state and, once fulfilled, the destination's name. Destination
settings stay administrator-only.

## Base-image requests

A developer or maintainer whose build needs a base image from an unlisted repository asks for it on the project
page (**Request a base image**), from the admission result in New image (a **Request** button per unlisted
repository), or with `POST /projects/{id}/base-image-requests` (`recipes-and-policies.md#base-image-requests`).
Organization administrators other than the requester decide it in **Approvals** with the normal decision. Approval
adds the repository to the `base-images` policy in the deciding transaction; rejection or expiry leaves the policy
unchanged.

## Egress access requests

A developer or maintainer whose build needs network access only an administrator can open presses **Request
access** on a readiness item or on the build page's blocked destinations, or calls `POST
/projects/{id}/egress-access-requests` (ADR 0052, `egress.md#project-allowances`). The server attaches evidence from
stored data and accepts only entries that evidence shows as requestable. Organization administrators other than the
requester decide it in **Approvals**, which shows the evidence and what approval opens, for which project and build
method. Approval creates project egress allowances for that project only; rejection records the reason; expiry
(14 days) closes the request. The organization egress policy never changes.

## Automatic publication

A project in automatic mode (`PATCH /projects/{id}`, maintainers and administrators) publishes when a build's
validation passes and its spec has `publication.mode: automatic` with a `destinationRef` naming a destination open to
the project. Samuori requests the publication itself (requester `system`), in the transaction that completes the
validation. Approvals and the publish-time gate still apply. A spec whose destination or tags are not allowed is
skipped, and the skip is audited.
