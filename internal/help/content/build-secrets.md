---
title: Secrets for your build
section: Building images
summary: Give a build a token or certificate without ever handling the value.
roles: [developer, maintainer]
order: 30
---
Builds get secrets by **name**. You never type or see a value.

## Use a key

1. In **New image**, open **Secrets the build needs**. If your Dockerfile mounts a secret
   (`RUN --mount=type=secret,id=...`), Samuori lists it and offers to add it.
2. For each secret, choose a key in the **Key** column. The list holds the keys an administrator granted to your
   project; a key set shows each of its keys by name, such as `mssql-base › sa_password`. Choosing one takes
   effect at once. If your secret IDs share names with keys in a set, **Use matching keys** binds them all.
3. The secret is **Ready**. The build receives the value only while it runs, as a file at `/run/secrets/<name>` or
   the path your Dockerfile names, and never in the image layers.

## No suitable key?

Use **Ask for binding**, say why the build needs it, and an administrator grants a key or binds it.

## Good to know

- Required secrets without a key block the build before it starts, with the reason.
- A secret mount only keeps the value out of the image. Anything your build **writes** using it can still end up in
  the image; Samuori's sensitive-content check looks for that. See [Validation results](help:validation).
- If an administrator revokes a key, builds that use it stop receiving it at once.
