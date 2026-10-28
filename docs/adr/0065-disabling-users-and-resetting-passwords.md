# 0065. Disabling users and resetting passwords

Status: **Accepted** (2026-10-28, owner direction to make people and access editable; provisional, may change during
acceptance testing). Extends the session rules of [ADR 0045](0045-cluster-lifecycle.md) and the recovery rules in
[identity](../architecture/identity.md#recovery-access).

## Context
Administrators could create users and grant roles, but could not take access away from a person other than by
removing roles, could not help someone who forgot a local password, and people could not change their own password.
Users cannot be deleted: audit events, uploads, approvals and publications name them, and their IDs must keep
resolving.

## Options
- **Removing a person:** disable (chosen); hard delete (rejected: it breaks audit attribution and foreign keys).
- **Disabled user's credentials:** revoke sessions and API tokens when disabling (chosen: a leaver's tokens never
  come back by accident); leave tokens suspended so they work again after re-enabling (rejected for people; it stays
  the rule for service identities, whose tokens are administrator-issued infrastructure credentials).
- **Administrator password reset:** a random temporary password shown once that must be replaced at the next
  sign-in (chosen: it reuses the shown-once pattern of API tokens and never leaves the administrator knowing the
  password the user ends up with); an administrator-chosen permanent password (rejected: the administrator knows it
  indefinitely); an emailed link (rejected: Samuori sends no mail for identity and has no verified addresses).
- **Forced change:** the sign-in request carries the new password (chosen: the change is checked under the same
  per-address limit and per-account lockout as every password sign-in, and no session exists until it succeeds);
  a restricted session that can only change the password (rejected: a new session state every route would have to
  honour).

## Decision
- **Disable** (`POST /users/{id}/state`, organization administrators): sets `users.disabled` and, when disabling,
  revokes all the user's sessions and API tokens in the same transaction. Disabled users are already refused at
  sign-in through every provider and by session and token authentication. Refused with 409: your own account
  (`own_account`), the last enabled organization administrator (`last_administrator`) and the last enabled local
  administrator (`last_local_administrator`), the same rules as revoking the administrator role. Re-enabling restores
  sign-in, not the revoked tokens. Audited `user.disable` and `user.enable`.
- **Reset** (`POST /users/{id}/password-reset`, organization administrators, local users only, not yourself): stores
  the hash of a random 24-character password with `user_credentials.temporary_until` 24 hours ahead (migration 00071),
  ends the user's sessions and returns the password once with `Cache-Control: no-store`. API tokens are kept; disable
  the user to stop them. Audited `user.password-reset`.
- **Sign-in with a temporary password:** after the usual checks, without `newPassword` the request fails with
  `403 password_change_required` and no session; with `newPassword`, which must meet the password policy and differ
  from the temporary one, the new password replaces it, `temporary_until` is cleared, and a session is issued.
  After `temporary_until` the temporary password is refused like a wrong one and an administrator resets again.
- **Self-service change** (`POST /me/password`, any person with a local password, from a browser session only):
  requires the current password, which is checked under the per-address rate limit and the per-account lockout of
  sign-in; wrong attempts count towards the lockout. The new password meets the policy and differs from the current
  one. Every other session of the user ends; API tokens are kept. Audited `user.password-change` (also when
  refused for a wrong current password).
- Password hashes are never returned by any endpoint.

## Consequences
- An administrator can lock someone out at once without losing their history.
- A reset or change ends sessions on other devices; tokens survive, so automation keeps running.
- Not yet: password expiry, password history, and administrator-forced sign-out without disabling.
- Reversible: dropping `temporary_until` makes reset passwords permanent; the endpoints can be removed without data
  loss.
