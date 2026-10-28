# 0064. Editing secret grants

Status: **Accepted** (2026-10-28, owner request to make the portal editable; provisional, may change during acceptance
testing). Extends the grant model in [`../architecture/secrets.md#grants`](../architecture/secrets.md#grants).

## Context
A grant fixes a project's access to one key: usage (build, test or both), version policy and eligible worker pools.
Until now the only way to change any of them was to revoke the grant, which disables every binding made from it, and
grant again, after which developers had to bind every spec secret ID again. Bindings copy the grant's scope when they
are made, and release checks the binding, not the grant, so an edit must decide what happens to them.

## Options
- **Revoke and re-grant (rejected):** safe, but breaks every build that uses the key for a change that may only widen
  access.
- **Edit the grant only (rejected):** bindings would keep the old scope, so a narrowed grant would not narrow what
  builds receive.
- **Edit in place; bindings follow; non-fitting bindings are disabled (chosen).**

## Decision
- `PUT /projects/{id}/secret-grants/{grantId}`, administrators only, like grant and revoke. The name, usage, version
  policy and eligible pools may change; the provider and reference may not (grant the other key instead). A reason is
  required. The same checks as creation apply: the version must exist, the pools must belong to the organization, the
  project must be active, and an external reference is read once to prove it still resolves. Revoked grants cannot be
  edited.
- **Covers.** A scope covers another when its usage is `both` or equal to the other's, and its pool list is empty
  (every eligible pool) or contains every pool of the other, which must itself name pools. For a key set, a binding's
  key must also still be among the key names, which are read again when the version policy changes.
- **Each active binding made from the grant**, in the same transaction:
  - if the new scope covers it, it stays **active** and takes the grant's new usage, pools and version policy. Its
    version is kept, so builds already admitted keep receiving it, unless the **version policy changed**: then the
    version is incremented, since the binding would now deliver a different value than the one a running build was
    admitted with;
  - otherwise it is **disabled**, with the reason `grant narrowed: <reason>` and a `secret.unbind` audit event, so
    admission refuses it and running builds can no longer receive it, exactly as for a revoke.
- Bindings made directly by an administrator, and bindings of other grants, are never touched.
- The change is audited as `secret.grant.update` with the before and after name, usage, version and pools, the
  number of bindings kept and disabled, and the reason. The response lists the disabled bindings.
- The portal computes the same rule before saving and warns, naming the spec secret IDs that would be disabled.

## Consequences
- Widening (a new pool, test usage added) no longer interrupts anyone.
- Narrowing is immediate, like a revoke but limited to bindings that no longer fit; developers bind another key.
- Unbinding stays administrator-only: developers stop using a key by binding another key or removing the secret
  from their spec.
- Evidence: `TestUpdateGrant`, `TestCovers`, `TestSecretAndImageSourceEdits`.
