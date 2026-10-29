# Identity

Status: **Implemented (WP27).** Product decision: D09. Threat rows: TM-08 (OIDC and LDAP sign-in manipulation),
TM-04 (tokens). Screen: Organization > Identity & sign-in.

Samuori signs people in with local passwords, OpenID Connect or LDAP/Active Directory. Groups from a provider map to
Samuori roles. Automation uses service identities with expiring API tokens. No identity provider is required: an
all-in-one install works with local accounts alone, and Keycloak or any other provider is optional (ADR 0016's
principle applied to identity).

## Providers

An administrator adds a provider under **Organization > Identity & sign-in** (`POST /identity-providers`). It starts **disabled**; test it,
add group mappings, then enable it. Every change is audited. The OIDC client secret or LDAP bind password is a
local secret that becomes **controller-only**, so it can never be bound to a build or read back. Type it in the
provider dialog (it is stored as `identity-<name>-bind-password` or `identity-<name>-client-secret`), or choose a
secret stored earlier under **Security > Secrets**.

| Kind | Settings | Notes |
|---|---|---|
| `oidc` | issuer URL, client ID, scopes (`openid` always), username, display name and groups claims (defaults `preferred_username`, `name`, `groups`) | The issuer must be `https` (plain `http` only on loopback, for testing). Needs `externalUrl` |
| `ldap` | One to eight `ldaps://` URLs separated by spaces, tried in order, or `ldap://` with StartTLS; CA certificate; bind DN; user base DN and filter with `{username}`; optional group base DN and filter with `{dn}` | Plain `ldap://` without StartTLS is refused except on loopback, so passwords never cross the network unencrypted |

For LDAP the **Directory** choice fills in defaults: OpenLDAP uses `(&(objectClass=person)(uid={username}))`, `uid`
and `cn`; Active Directory uses `(&(objectClass=user)(sAMAccountName={username}))`, `sAMAccountName`, `displayName`,
and the group filter `(member:1.2.840.113556.1.4.1941:={dn})`, which also finds nested groups. Every field stays
editable. Paste the directory's CA certificate when the controllers do not already trust it.

Each domain controller's certificate must name the host in its URL, so list every controller rather than the
domain name unless the certificates include it. **Edit** changes a provider's label and settings and, if a new
one is typed, its secret (stored under a new name, since stored values never change); the kind and name are fixed,
and the edit is audited. Retest after editing.

Group mappings match the group names the sign-in collects: for LDAP, each `memberOf` DN and its first `cn`, plus
the group search's name attribute (default `cn`), compared case-insensitively as directories do. OIDC groups match
the claim exactly.

Set `externalUrl` (for example `https://samuori.example`) before adding an OIDC provider. Register
`<externalUrl>/api/v1/auth/oidc/callback` with the provider; Organization > Identity & sign-in shows it.

## OIDC sign-in

The authorization code flow with PKCE (`internal/adapters/identity/oidc`, using coreos/go-oidc):

1. **Start:** `GET /auth/oidc/{provider}/start` creates random `state`, `nonce` and PKCE verifier values and stores
   them server-side. The `state` is stored only as a hash. The record is single-use and expires after ten minutes.
   It is bound to the browser by a random value in an `HttpOnly` cookie, `SameSite=Lax` so it survives the
   provider's cross-site redirect. The browser goes to the provider with an S256 code challenge.
2. **Callback:** `GET /auth/oidc/callback` marks the state used before anything else, so a replayed state fails.
   It requires the same browser cookie and exchanges the code with the verifier. The ID token must verify:
   signature against the provider's published keys (`alg: none` is refused), issuer, audience (the client ID),
   expiry and nonce.
3. **Result:** a session like a password sign-in, then a redirect to the web UI. Any failure redirects to the
   sign-in page with one generic message. The audit log records the cause.

Starts are rate-limited per address in their own budget, separate from password attempts.

## LDAP sign-in

The sign-in page shows each enabled LDAP provider as a tab, with a button per OIDC provider above; with a single
provider it names the account the form expects. While any provider is enabled the **Local account** tab appears only
after **Local administrator sign-in** (or at `/?local`; see [Recovery access](#recovery-access)).
Hiding is presentation only: the server still accepts local passwords as `localLogin` allows. The controller:

1. binds as the service account and searches the user base. The username is **escaped** before it replaces
   `{username}` (SEC-AUTH-06), and exactly one entry must match;
2. binds as that entry with the given password; an **empty password is refused before any bind**, because LDAP
   treats it as an anonymous bind;
3. reads groups from `memberOf` (both the DN and its first `cn`) and from the optional group filter (the user DN,
   escaped, replaces `{dn}`).

LDAP sign-ins share the password rate limit and the per-account lockout.

## Users and roles

- **Provisioning:** a person's first sign-in creates a Samuori user linked to the provider and its stable subject.
  OIDC uses `sub`; LDAP uses `entryUUID` or `objectGUID`, so a renamed entry stays the same user. **An existing
  account with the same username is never linked automatically**; the sign-in is refused and audited.
- **Group mappings** (`/identity-providers/{id}/group-mappings`) grant a role at organization, team or project
  scope to members of a group. Bindings from mappings are **replaced at every sign-in**: leave the group and the
  role goes at the next sign-in. Bindings an administrator grants by hand are never touched. Sign-ins and role
  changes are audited (`identity.sync`).
- **Disabled users** cannot sign in through any provider.
- **Sessions** follow the same rules as for local users ([Sessions](#sessions)). Disabling a provider stops new
  sign-ins.

## Sessions

Browser sessions are server-side rows behind an httpOnly cookie, with a CSRF token for every state-changing request
(ADR 0045, after Vault's model).

- **Two deadlines.** The idle deadline is the last activity plus the idle timeout, never later than the absolute
  deadline, which is the sign-in time plus the maximum session. The server refuses a session past either.
- **What counts as activity.** Only state-changing requests and `POST /auth/session/extend` renew the idle deadline.
  Reads never do, so background polling and `GET /auth/session`, which reports both deadlines, keep nothing alive.
  Extending a session past either deadline is refused, and the absolute deadline is never extended.
- **Renewal is re-authentication (ADR 0068).** Before the maximum, `POST /auth/session/renewal` (session cookie and
  CSRF token; API tokens are refused) lets the session's own user prove who they are again and replaces the session
  with a new one: a new session ID and CSRF token, new idle and absolute deadlines, the same user. The old session is
  revoked in the same transaction, the new cookies carry the sign-in flags, and `session.renew` is audited, as are
  refusals. The maximum still bounds each session; renewal never moves a deadline of the old one.
  - *Password accounts* (local and LDAP) send the current password. It is checked by the sign-in path, under the same
    per-address rate limit and account lockout, and wrong attempts count toward the lockout. A wrong password answers
    400 `wrong_password`, never 401, so the page does not sign out. Disabled users, temporary passwords that must be
    changed and local passwords limited to administrators are refused as at sign-in. LDAP checks the stored username
    and must resolve to the same user.
  - *OIDC accounts* get 202 with the provider URL; the browser goes to the provider and comes back through the
    callback, which replaces the renewed session only when the provider asserts the same subject. Another account is
    refused (`/?signin=failed`) and the session stays as it was. If the renewed session ended during the round trip,
    the person has just signed in, so the callback issues an ordinary session.
  - An ended session cannot be renewed (401 from the guard, or 404 `no_session`): the person signs in again.
- **Return to the page.** OIDC sign-in and renewal take `returnTo`, accepted only as a same-origin path of the web UI
  (starts with one `/`, no backslash, whitespace or control characters, no scheme or host, not under `/api`) and
  stored with the sign-in state, so the callback can only redirect within the site; anything else returns to `/`
  (the sign-in start) or is refused with 400 `invalid_return_to` (renewal).
- **Portal.** Clicks, keys and back/forward navigation renew the session quietly, at most once a minute and only
  after half of the idle window has passed. The warning lead before the idle deadline opens a dialog with a
  countdown, **Stay signed in** and **Sign out**. Ten minutes before the absolute deadline (or the warning lead, if
  longer) a notice says "Your session ends at HH:MM. Renew it to keep working." with **Renew session** and
  **Dismiss**. **Renew session** opens a small dialog: a password field, or **Continue with** the provider, plus
  **Sign out**. A password renewal closes the dialog, refreshes the deadlines in every tab and briefly confirms the
  new end time; nothing on the page reloads. A provider renewal loads the provider's page and comes straight back to
  the same page; wizard drafts are already saved on the server as the person works, so they survive it, but text
  typed into other open forms does not. On expiry the sign-in page says why, and signing in returns to the same page
  (in place for passwords, through `returnTo` for OIDC), still reloading onto a newer release when one was
  installed. Tabs of one browser share deadlines, extensions and sign-outs through a `BroadcastChannel`, with a
  `localStorage` event fallback, so one tab extends or renews for all.
- **Settings.** Administrators set the idle timeout (15–120 minutes, default 30), the maximum session (1–24 hours,
  default 12, not shorter than the idle timeout) and the warning lead (30–600 seconds, default 120) on the Identity
  page (`PUT /identity-settings`, `session`). They are stored per organization, apply to existing sessions at once
  and are audited (`identity.session-timeouts`). Extensions are not audited.

## Recovery access

`PUT /identity-settings` with `localLogin: administrators` keeps local passwords only for organization
administrators. Everyone else must use a provider. Each administrator's local sign-in is then recorded as a
**recovery** sign-in in the audit log. The default is `all`. When a provider is enabled, local sign-in is behind
**Local administrator sign-in** on the sign-in page (or `/?local`), the break-glass path when the directory or SSO is
down.

Samuori keeps one local administrator: revoking the organization administrator role from the last enabled local
(password) user is refused with `409 last_local_administrator`, even when directory administrators remain, so the
organization stays reachable when every provider fails. Organization > Users shows each user's source and which roles
come from group mappings.

## Managing people and teams

All of these are for organization administrators only, checked on the server, and audited (ADR 0065).

- **Display names:** `PATCH /users/{id}`; people change their own with `PATCH /me` (Account page). Usernames never
  change. Audited `user.update`.
- **Disable, never delete:** users stay so the audit log, uploads and approvals keep naming them.
  `POST /users/{id}/state` with `enabled: false` revokes every session and API token of the user in the same
  transaction, and sign-in through any provider is refused. Refused with 409: your own account, the last enabled
  organization administrator and the last enabled local administrator. Re-enabling restores sign-in, not the
  revoked tokens.
- **Password reset** (local users, not yourself): `POST /users/{id}/password-reset` returns a random temporary
  password once (`Cache-Control: no-store`) and ends the user's sessions. It is valid for 24 hours and only to choose
  a new password: signing in with it answers `403 password_change_required` until the request also carries
  `newPassword`, which must meet the policy. API tokens are kept.
- **Own password** (`POST /me/password`, from a browser session): needs the current password, checked under the
  sign-in rate limit and lockout. Every other session ends; tokens are kept.
- **Change role:** `PATCH /role-bindings/{id}` replaces the role and keeps the scope, in one transaction audited
  `role.change` with the old and new role. The last-administrator rules of revoke apply; bindings from group mappings
  are refused (change the mapping).
- **Teams:** `PATCH /teams/{id}` renames under the creation rules (unique, lower-case with dashes); `DELETE
  /teams/{id}` succeeds only when no project (active, archived or purged), role binding, group mapping, notification
  rule or image source refers to the team, and otherwise answers `409 team_in_use` naming what does.
- **Identity providers:** `DELETE /identity-providers/{id}` removes a provider that never signed anyone in, with its
  group mappings; once a user is linked to it the answer is `409 provider_in_use` and the provider can only be
  disabled.
- **Service identities:** `PATCH /service-identities/{id}` changes the display name; they are never deleted because
  pipelines name them as owners.

## Service identities

`/service-identities` are accounts for automation. They have no password and no provider, so they cannot sign in.

- An administrator issues them expiring API tokens: read-only or read-write, for at most a year. The value is shown
  once and stored as a hash.
- Roles are ordinary role bindings.
- Disabling an identity stops its tokens at once; re-enabling makes them work again. Creation, renaming, token issue
  and revocation, and disabling are audited.
- Requests made with their tokens carry the `service` principal kind.

## Evidence

> History: the tests named below ran at earlier commits and were removed (ADR 0039); restored focused tests are
> marked in `docs/developers/security-test-catalog.md`.

- `internal/identity`, with the real OIDC adapter against an in-process provider (`internal/testsupport/oidctest`)
  that enforces PKCE. **SEC-AUTH-05** refuses each of these, with the cause in the audit log:
  - a wrong issuer, audience or nonce;
  - an expired token, an unknown signing key or `alg: none`;
  - a sign-in finished in another browser;
  - a forged code, and a reused, unknown or expired state.

  The same tests cover provisioning and group resync, manual bindings surviving resync, refusing to link an
  existing username, provider and mapping validation, LDAP refusing empty passwords, and service identities.
- `internal/adapters/identity/ldap`: filter escaping against injection strings (**SEC-AUTH-06**). In the dev VM
  (`deploy/dev/lima/provision-ldap.sh`, `make test-buildkit`), the tests start a throwaway OpenLDAP 2.6.10 server
  over LDAPS with a test CA. Against it they check:
  - the service bind and a wrong bind password;
  - a real user, display name, `entryUUID` subject and group membership;
  - injection usernames, another user's password and an empty password;
  - a CA that did not sign the server certificate.
- `internal/api/identity_test.go`: administrator-only routes and the public provider list. A full OIDC sign-in over
  HTTP with cookies gets the mapped role, a callback in another browser fails, the recovery-only policy refuses a
  developer's password and records the administrator's, and a service token works until the identity is disabled.
- `ui/e2e/08-identity.spec.ts`: in Chromium, an administrator adds an OIDC provider through the UI, tests it, maps a
  group and enables it. A person then signs in through the provider on another site (`localhost` versus
  `127.0.0.1`), lands on the home page and has the mapped role.

- `internal/auth/sessions_test.go` and `internal/api/session_test.go` (**SEC-AUTH-11**): reads and the status check
  leave the idle deadline alone; extending stops at the absolute deadline; expired sessions cannot be extended or
  used; only administrators change the timeouts; out-of-bounds values are refused; a shorter idle timeout applies to
  existing sessions.
- `internal/auth/renewal_test.go` and `internal/api/session_renewal_test.go` (**SEC-AUTH-13**): renewing with the
  right password gives a new session and CSRF token with new deadlines and an audit event, the old cookie is then
  refused and cannot renew again; another user's password, a wrong password (which counts toward the lockout and
  then locks renewal and sign-in) and the per-address rate limit are refused without ending the session; an expired
  session, a disabled user, a temporary password, an API token and a request without CSRF are refused; the cookies
  keep their flags. Over HTTP with a fake OIDC client: renewal returns the provider URL, the callback replaces the
  session and returns to `returnTo`, a different subject is refused and leaves the session alone, and absolute or
  protocol-relative `returnTo` values return to `/` or are refused. `ui/src/session/renewal.test.ts` covers the
  notice timing and the client-side return path.

- `internal/api/people_test.go` (**SEC-AUTH-12**), over HTTP with real sessions on SQLite: disabling a user ends
  their session and API token at once and refuses sign-in, re-enabling does not revive the token, and disabling
  yourself or the last local administrator is refused; an administrator reset ends the user's session, the old
  password stops working, the temporary password alone gets `password_change_required`, a weak replacement is
  refused, a good one signs in and the temporary password cannot be reused, an expired one is refused, and the
  response is `no-store`; changing your own password needs the current one, ends the other session but not this
  one, is refused with an API token and locks after five wrong attempts; role change keeps the scope and is audited
  with both roles, and the last administrator cannot be demoted; team rename and delete, including 409 for a role
  binding and for a purged project; provider delete with 409 once a user is linked; and 403 for a team-scoped
  administrator on every one of these operations.

## Limitations

- Not yet tested against a commercial OIDC provider or Active Directory. `objectGUID` handling and AD-specific
  filters have been tested only by construction.
- No SCIM, no OIDC back-channel logout, no refresh of group membership between sign-ins.
- LDAP StartTLS is implemented but only LDAPS is tested live.
- One external URL per controller; several hostnames need a reverse proxy.
- The session warning, the renewal dialog, cross-tab sharing and return-to-page behavior have no browser test yet;
  OIDC renewal is tested with a fake provider client, not a real provider, and LDAP renewal only by construction.
