# Exporting and importing configuration

An organization administrator can save the organization's configuration as one YAML file and apply it to another
installation, or to the same one later. The file holds settings and names only, never secrets. Design and rules:
[ADR 0069](../adr/0069-exporting-and-importing-configuration.md). For a full copy with secrets, use
[backups](backup-restore.md) instead.

## What the file holds

| Section | Contents | Matched by |
|---|---|---|
| `teams` | Names of teams that notification rules refer to | name |
| `identity` | Local sign-in (`all` or `administrators`) and session timeouts | one per organization |
| `auditRetention` | Retention days, scheduled export, purge only exported | one per organization |
| `policies` | The current document of each policy kind, egress presets and destinations included | kind |
| `buildMethods` | Allowed and default build methods; provider settings with pools by name and projects as `team/project` | provider name |
| `recipes` | Built-in recipe and shipped-runtime statuses that are not approved, with reasons; draft and approved organization runtimes; draft, approved and deprecated custom recipe versions with definitions | recipe ID, version and runtime label |
| `workerPools` | Name, location, isolation profile, trust domain, trusted workloads only | name |
| `registries` | Kind, endpoint (or ECR account and region), plain HTTP, private network | name |
| `credentialProfiles` | Name, registry, type, purpose, repository scopes; no username or secret | name |
| `destinations` | Registry, repository, push credentials, allowed projects, protected and immutable tags, approval, signing, provenance, promotion only | name |
| `notificationChannels` | Kind, webhook URL or email recipients, private network, enabled; no signing key or Slack or Teams URL | name |
| `notificationRules` | Channel, events, team or project | channel, scope and events |

Never in the file: secret values, secret providers, credentials' usernames and secrets, keys, certificates, tokens,
users, service identities, role bindings, identity providers, sessions, workers and their enrollment, projects and
everything in them (sources, specs, builds, images, publications), approvals, image sources, signing keys, cluster and
federation material, and audit records.

## Export

Portal: **Organization > Export & import > Download configuration**. CLI: `samuori config export -o
configuration.yml` (with `--json` for JSON). API: `GET /api/v1/configuration/export`. Each export is audited as
`configuration.export`.

```yaml
apiVersion: samuori.invalid/v1alpha1
kind: Configuration
metadata:
  exportedAt: "2026-10-29T08:00:00Z"
  samuoriVersion: 0.1.0
  organization: example
spec:
  workerPools:
    - name: builders
      location: lab
      isolationProfile: rootless-dedicated-vm
      trustDomain: builders.example
  registries:
    - name: lab
      kind: oci
      endpoint: registry.example.com
  credentialProfiles:
    - name: lab-push
      registry: lab
      type: oci-basic
      purpose: push
  destinations:
    - name: web
      registry: lab
      repository: team/web
      pushProfile: lab-push
      allowedProjects:
        - team-a/web
      protectedTags:
        - v*
  notificationChannels:
    - name: ops-mail
      kind: email
      recipients:
        - ops@example.com
      enabled: true
  notificationRules:
    - channel: ops-mail
      events:
        - build.failed
      project: team-a/web
```

The file is checked against `api/schemas/configuration.v1alpha1.json`; edit it if you like, and keep it under change
control. Lists are sorted, so two exports of the same configuration differ only in `exportedAt`.

## Import

1. **Preview.** Portal: choose the file under **Import**. CLI: `samuori config import configuration.yml`. API:
   `POST /api/v1/configuration/import` with the file as `application/yaml`. Nothing changes. Each item is listed as
   `create`, `update`, `unchanged` or `skip` with a reason, and **needs attention** lists what you must do by hand.
2. **Apply.** Portal: **Apply import** and confirm. CLI: add `--apply`. API: `?dryRun=false` with an
   `Idempotency-Key` header. Each change goes through the same checks as in the portal and writes its usual audit
   event; the import as a whole is audited as `configuration.import`.
3. **Finish by hand.** Do what needs attention, then import the same file again: what was applied is unchanged, and
   what waited (destinations for new credentials, rules for new channels) is added.

With the example above on an installation that has team `team-a` with project `web` and nothing else, the preview
shows:

| Item | Change | Why |
|---|---|---|
| workerPools `builders` | create | workers enrol and pass self-tests before it runs builds |
| registries `lab` | create | |
| credentialProfiles `lab-push` | skip | `oci-basic` credentials hold a secret, which is never exported |
| destinations `web` | skip | needs registry `lab` and credentials `lab-push` here first |
| notificationChannels `ops-mail` | create | |
| notificationRules `ops-mail: build.failed for team-a/web` | create | |

After applying, add credentials `lab-push` with their password under **Registries** and import again; the destination
`web` is then created.

## Rules to know

- Nothing missing from the file is deleted, and no user, role binding, secret or project is created.
- Projects are referred to as `team/project` and must exist. A destination is imported without projects that do not
  exist (narrower access); a provider restricted to a pool or project that does not exist, and a notification rule for
  a project or team that does not exist, are skipped, because without the restriction they would apply more widely.
- Only anonymous credentials and email channels are created, since everything else needs a secret. Existing
  credentials are never changed.
- A policy change that would newly block existing specs is skipped; make it on **Policies**, which shows the impact
  and asks for a reason.
- A pool's isolation profile and trust domain, a registry's kind and a destination's registry are not changed by an
  import; change them where they are edited.
- Approved organization runtimes and custom recipe versions never change, and an import never moves a status back.
- When an item fails, the others still run and the answer says which failed and why (`complete: false`; the CLI exits
  with 1). Fix the cause and import again.
