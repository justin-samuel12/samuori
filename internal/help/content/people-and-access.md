---
title: Teams, users and roles
section: Administration
summary: Create teams and local users, and give people roles.
roles: [administrator]
order: 200
---
## Teams

On **Teams**, **New team** creates a team. Projects belong to teams, and a role held on a team covers all of its
projects.

## Users

On **Users**, **New user** creates a local account with a password of at least 12 characters that does not contain
the username. People who sign in through LDAP, Active Directory or OIDC get an account on their first sign-in; see
[Sign-in and directory groups](help:identity).

## Give a role

On a user's row, **Grant role**: choose the role and the scope (organization, team or project). **Remove** takes it
away at once. What each role can do is in [Roles and access](help:roles).

Prefer team scope: it gives exactly the access a team needs. Keep administrator roles few, and keep one local
administrator account for recovery.
