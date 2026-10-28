---
title: Custom recipes
section: Administration
summary: Define your organization's own builds once, so developers pick them and fill in a few values.
roles: [administrator]
order: 225
---
A custom recipe is a build your developers choose in **New image** without writing a Dockerfile. You define it once
under **Recipes & base images > Custom recipes**; Samuori turns it into a Dockerfile itself.

## Create one

1. **New recipe** opens a YAML editor with a working example.
2. Describe the recipe: `id`, `version`, `displayName`, `platforms`, and optionally `languages` and
   `applicationTypes` (leave them out to offer it for any application).
3. Add **parameters** developers fill in: `string`, `version`, `port` or `path`, each with a default or marked
   required.
4. List the **secrets** its steps mount and the **egressPresets** it downloads from.
5. Write the **stages**. Each starts from an `image` pinned by digest, or `from` an earlier stage, and has steps:

| Step | Example |
|---|---|
| `workdir` | `- workdir: /app` |
| `copySource` | `- copySource: {from: ., to: /app}` |
| `copyFromStage` | `- copyFromStage: {stage: build, from: /app, to: /app, owner: "10001"}` |
| `run` | `- run: {shell: "apt-get update", network: egress}` or `{argv: [make, build], secrets: [npm_token]}` |
| `env` | `- env: {name: APP_MODE, value: production}` |
| `user` | `- user: "10001"` |
| `expose` | `- expose: "${port}"` |
| `label` | `- label: {name: org.opencontainers.image.title, value: "${name}"}` |
| `cmd` | `- cmd: [node, server.js]` |

Use `${name}` for a parameter in values, and `$name` inside shell scripts. The final stage must end with a non-root
`user`, unless you add `runsAsRoot:` with the reason. Copied source is owned by root, so a non-root final stage
starts from a fresh image and copies the application in with `copyFromStage` and an `owner`, as the starter recipe
does. Otherwise the runtime check fails with a permission error.

6. **Preview** shows the Dockerfile Samuori will build, or exactly what is wrong. **Save draft** keeps it.

A draft can be edited as often as you like. **Discard draft** deletes one you no longer need, after you confirm;
it cannot be recovered.

## Approve and maintain

**Approve** shows the Dockerfile once more and asks for a reason. After that, developers can use the version, and it
never changes: **New version** copies it with the next version number. **Deprecate** warns developers; **Withdraw**
refuses new builds. Approved versions can't be edited or discarded; they stay as history. Every step is recorded
in the audit log.

## Runtimes you add to built-in recipes

Under **Runtimes**, an approved runtime can't change either. **Duplicate** opens the add form with its images and
note filled in and saves a new draft; give it another runtime version, or withdraw the original first.

Builds from custom recipes go through the same checks as any build: base-image and image-source rules, network
rules, secret grants, validation and publication.

## Built-in recipes and their runtimes

Built-in recipes come with Samuori. Under **Recipes & base images**, **Change status** deprecates or withdraws a
whole recipe version. **Runtimes** lists the runtime versions it ships, for example Node.js 22 and 24, and lets you
act on one of them with a reason:

- **Deprecate**: builds still run, with a warning that shows your reason. New image marks it as deprecated.
- **Withdraw**: developers no longer see it. A spec that asks for exactly that version is refused with your reason;
  a range such as `>=22` uses the newest runtime still available.
- **Approve again**: offers it as before.

Your decisions belong to your organization and are kept when Samuori is updated. They apply to new builds; builds
already queued are not changed. Every decision is recorded in the audit log.
