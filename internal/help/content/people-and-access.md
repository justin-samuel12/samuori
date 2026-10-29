---
title: Teams, users and roles
section: Organization
summary: Create teams and local users, and give people roles.
roles: [administrator]
order: 200
---
## Teams

On **Organization > Teams**, **New team** creates a team. Projects belong to teams, and a role held on a team covers all of its
projects.

**Rename** changes a team's name under the same rules. **Delete** works only for a team nothing refers to: no
projects (archived and purged ones count, because they keep their records), role bindings, group mappings,
notification rules or image sources. Otherwise the message says what is still there.

## Users

On **Organization > Users**, **New user** creates a local account with a password of at least 12 characters that does not contain
the username. People who sign in through LDAP, Active Directory or OIDC get an account on their first sign-in; see
[Sign-in and directory groups](help:identity).

On a user's row:

- **Edit name** changes the display name; the username never changes.
- **Reset password** (local accounts) signs the user out and shows a temporary password once. Give it to them over
  a secure channel: it works for 24 hours and only to choose a new password at the next sign-in. Their API tokens
  keep working.
- **Disable** signs the user out everywhere, revokes their API tokens and stops every sign-in. Users are never
  deleted, so the audit log keeps naming who did what; **Enable** lets them sign in again, with new tokens. You
  cannot disable yourself or the last administrator, and one local administrator always stays enabled.

## Give a role

On a user's row, **Grant role**: choose the role and the scope (organization, team or project). **Remove** takes it
away at once. **Change role** swaps the role on the same scope in one step. Roles that come from a directory group
are changed through the group mapping instead. What each role can do is in [Roles and access](help:roles).

Prefer team scope: it gives exactly the access a team needs. Keep administrator roles few, and keep one local
administrator account for recovery.
