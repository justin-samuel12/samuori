# 0068. Renewing a session by re-authenticating

Status: **Accepted** (2026-10-28, owner direction to make reaching the maximum session seamless; provisional, may
change during acceptance testing). Extends the session rules of [ADR 0045](0045-cluster-lifecycle.md) and
[identity](../architecture/identity.md#sessions).

## Context
A browser session has an idle deadline and an absolute deadline, the maximum session (1 to 24 hours). The absolute
deadline is never extended, so a person working when it arrived was signed out and had to sign in again on a new
page load, losing what they had not saved. The maximum exists to bound how long one authentication is trusted, not
to interrupt people who can prove again who they are.

## Options
- **Extend the absolute deadline on activity** (rejected: the maximum would no longer bound anything, and a stolen
  session could be kept alive indefinitely).
- **Extend it after re-entering the password, keeping the session** (rejected: the session ID would outlive the
  authentication that issued it, and the deadline rules would need an exception).
- **Re-authenticate into a new session** (chosen): proving identity again is a sign-in, so it gets a new session ID,
  a new CSRF token and new deadlines, and the old session is revoked. The maximum still bounds each session.
- **SSO renewal in a pop-up or hidden frame** (rejected for now: providers often refuse framing, pop-ups are
  blocked, and `prompt=none` silent renewal differs by provider). A full-page round trip back to the same page works
  with every provider.
- **Return path in browser storage only** (rejected for the OIDC round trip: the server must decide where its
  callback redirects, so the path is validated and stored with the sign-in state).

## Decision
- `POST /auth/session/renewal`, any signed-in person, browser session plus CSRF token only (API tokens: 403). An
  ended session cannot be renewed; the guard answers 401 and the person signs in again.
- **Password accounts** (local and LDAP) send the current password. It is checked like sign-in: the per-address rate
  limit, the account lockout with wrong attempts counted, disabled users, temporary passwords and the local sign-in
  policy. Only the session's own user is checked (the password is verified against that user, and LDAP must resolve
  to the same user), so a renewal can never switch accounts. A wrong password answers `400 wrong_password`, not 401,
  so the portal does not treat it as the session ending.
- On success, in one transaction: the old session is revoked, a new one is inserted for the same user and
  `session.renew` is audited. The response sets new session and CSRF cookies with the sign-in flags (httpOnly,
  Secure when configured, SameSite Strict) and returns the new deadlines. Refusals are audited `session.renew`
  `denied`.
- **OIDC accounts** get `202` with the provider URL. The sign-in state (migration 00072) records the session being
  renewed, its user and a validated `returnTo`. The callback, which receives no session cookie because it is a
  cross-site navigation, replaces that session only if the provider asserts the subject linked to the same user;
  otherwise it redirects to `/?signin=failed` and the session is unchanged. If the renewed session ended during the
  round trip, the person has just authenticated, so an ordinary session is issued.
- **`returnTo`** is accepted only as a same-origin web UI path: a single leading `/`, no backslash, whitespace or
  control characters, no scheme, host or user information, and not under `/api`. The sign-in start falls back to `/`;
  renewal refuses anything else with `400 invalid_return_to`. The callback validates the stored value again before
  redirecting.
- `GET /auth/session` reports `renewWith` (`password` or `provider`) and the provider's name, so the portal shows a
  password field or "Continue with" the provider.
- The portal shows the renewal notice ten minutes before the maximum (or the idle warning lead, if that is longer).

## Consequences
- People keep working past the maximum without leaving the page for password accounts; OIDC accounts make one round
  trip to the provider and come back to the same page. Wizard drafts are already saved on the server; other unsaved
  form text is lost on that round trip, and the dialog says so.
- Renewal is as strong as sign-in: a stolen session alone cannot renew without the password or the provider.
- No new client storage of sensitive data: the password goes only in the renewal request.
- Reversible: removing the endpoint and the three `oidc_logins` columns returns to sign-in-again behaviour.
