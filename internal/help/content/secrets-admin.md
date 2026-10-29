---
title: Secrets and project access
section: Security
summary: Store secrets, connect external stores, and grant keys to projects.
roles: [administrator]
order: 220
---
**Security > Secrets** has four tabs.

## Stored secrets

Secrets kept in Samuori's own encrypted store. Values are never shown again after you save them.

- **Store a secret:** a name, text or a binary file.
- **New version:** stores the next version under the same name and type. Keys granted with "latest" use it from the
  next build; keys pinned to a version keep theirs. Old versions are kept.
- **Delete:** destroys every version. It is refused while a project has the secret granted or bound, or while Samuori
  uses it for its own credentials; the message lists each use so you know what to revoke first.

## External stores

Connect OpenBao, a Vault-compatible server or AWS Secrets Manager. Values stay in the external store and are read
only when a running build needs them. **Test** checks the connection without reading a secret.

## Allowed secrets

The secrets of external stores that may be granted. **Edit** changes an entry's label and note; grants already made
keep their own names. **Remove** stops new grants of it.

## Project access

Choose a project, then **Grant a key**: a stored secret, or a path and key (or ARN) in an external store, with a
name developers see. For OpenBao or Vault you can also grant a **key set**: enter the path without `#key`, such as
`operations/mssql-base`. Samuori records the secret's key names, never its values, and developers pick keys from the
set. **Refresh keys** picks up keys added or removed in Vault; bindings to removed keys stop at once. The project's developers and maintainers then choose from these keys for each secret their
build uses, in the image wizard. They never see values or store paths.

**Edit** changes a granted key's name, usage, version and worker pools without revoking it. Widening (adding test
usage or a pool) keeps every binding working. Narrowing disables, at once, the bindings that no longer fit, and the
dialog names them before you save; developers then bind another key. Changing the version gives the bindings a new
version, so builds that started before the change can no longer receive the value.

**Revoke** withdraws a key and stops every build that uses it at once. **Secrets in use** shows which spec secret
uses which key. **Rotate** gives a binding a new version, for example after its value changed in an external store:
builds that started earlier stop receiving it, new builds use it. **Disable** stops a single one. Developers cannot
disable bindings themselves; they choose another key or remove the secret from their spec, or ask you.
