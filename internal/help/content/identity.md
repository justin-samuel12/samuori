---
title: Sign-in and directory groups
section: Organization
summary: Connect LDAP, Active Directory or OIDC, and map directory groups to Samuori roles.
roles: [administrator]
order: 210
---
**Organization > Identity & sign-in** connects Samuori to your directory so people sign in with their company account and get roles from their
groups.

## Add a provider

1. **Add identity provider** and choose LDAP (including Active Directory) or OIDC.
   - **LDAP:** an `ldaps://` URL, or `ldap://` with StartTLS; the CA certificate if your directory uses a private CA;
     a service account; and the user base and filter, such as `(sAMAccountName={username})` for Active Directory or
     `(uid={username})` for OpenLDAP. Add a group filter if your directory does not fill in `memberOf`.
   - **OIDC:** the issuer URL, client ID and secret. Register the callback URL the page shows with your provider.
2. The provider starts **disabled**. Test it.
3. **Add mapping:** members of a directory group get a role at a scope. Name the group by its full DN or its `cn`.
4. Enable the provider. The sign-in page then offers it.

**Delete** removes a provider only while nobody has signed in through it. After that, people's accounts stay linked
to it; disable it instead.

## How group roles behave

- Roles from mappings are recalculated at every sign-in: leave the group and the role goes at the next sign-in.
  Roles you grant by hand are never changed by this.
- An existing local account with the same username is never linked automatically; that sign-in is refused and
  audited.
- Under **Sign-in** you can keep local passwords for administrators only, once directory sign-in works.

## Sessions

Under **Sessions** you set how long the portal stays signed in:

- **Sign out after inactivity**, 15 to 120 minutes. Clicking or typing renews it; pages that refresh on their own do
  not.
- **Maximum session**, 1 to 24 hours. Activity never extends it. Ten minutes before it ends, people see **Renew
  session**: they confirm who they are (their password, or their identity provider) and get a new session that
  starts its own maximum, without leaving the page.
- **Warn before signing out**, 30 to 600 seconds. People then see a countdown with **Stay signed in**.

Changes apply to sessions already open and are audited.

## Service identities

For automation: **Add service identity**, give it roles like a user, then **Issue token**. Tokens expire within a
year and are shown once. **Edit name** changes the display name. Service identities are never deleted, because
pipelines name them as owners; disable one to stop its tokens.
