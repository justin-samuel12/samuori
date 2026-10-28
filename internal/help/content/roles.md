---
title: Roles and access
section: Reference
summary: The six roles, the scopes they apply to, and how access is decided.
roles: [everyone]
order: 90
---
Every permission in Samuori comes from a **role** held at a **scope**. You can hold several; your permissions add up.

## Roles

| Role | For | Can |
|---|---|---|
| **Developer** | People who build images | Upload source, edit build specifications, start and cancel builds, read logs, choose granted secret keys |
| **Maintainer** | Project leads | Everything a developer can, plus create and edit projects, request publication, retire or deprecate images, request revalidation and output exceptions |
| **Publisher** | Release managers | Request and retire publications and decide approvals. Publishers do not upload or build |
| **Operator** | People who run Samuori | Manage workers (cordon, drain, self-tests), the build queue, cancel builds, see system and upgrade status |
| **Security reviewer** | Security teams | Read the audit log, review policies, release quarantined images, decide approvals |
| **Administrator** | Whoever sets Samuori up | Users, teams, sign-in providers, registries, secrets, workers, backups and upgrades |

Roles do not include each other: an administrator is not automatically a developer.

## Scopes

- **Organization:** everything in the organization.
- **Team:** the team and every project in it. This is the usual scope.
- **Project:** one project.

## How access is decided

The Samuori server checks every action: the role must allow it, and the role's scope must cover that project or team.
Hiding a page is never what protects it, and these guides are filtered the same way.

Roles come from an administrator, under Administration > Users, or from your groups in the company directory
(LDAP, Active Directory or OIDC), refreshed each time you sign in.

## Your account

On **Account**, **Edit name** changes how your name is shown. With a local password, **Change password** asks for
the current one; your other signed-in browsers are signed out, and your API tokens keep working. If an administrator
reset your password, sign in with the temporary one and choose a new password on the sign-in page.

A session lasts at most the time your administrator set. Shortly before it ends, a notice offers **Renew session**:
enter your password, or continue with your identity provider, and you keep working on the same page. If a session has
already ended, signing in again brings you back to the page you were on.

**API tokens** on Account let scripts call the API as you. The [API reference](/help/api-reference) lists every
operation, the roles it needs and a `curl` example; send the token as `Authorization: Bearer <token>`.
