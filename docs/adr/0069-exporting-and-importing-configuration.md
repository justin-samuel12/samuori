# 0069. Exporting and importing configuration

Status: **Accepted** (2026-10-29, owner direction to move an installation's configuration to another installation;
provisional, may change during acceptance testing). Builds on [ADR 0066](0066-api-stability-and-deprecation.md) for
the API rules and on the services each section already has.

## Context
An administrator who sets up a second installation (a staging copy, a new site, a rebuild) had to repeat every policy,
registry, pool and notification by hand, and nothing showed how two installations differ. Backups (ADR 0035) copy
everything, secrets included, and only restore into an empty installation. What was missing is a reviewable file of
settings, without secrets, that can be applied to an installation that already runs.

## Options
- **Scope:** configuration an organization administrator manages (chosen); everything an organization holds,
  projects included (rejected: projects, specs and builds are work, not settings, and carry uploaded source); only
  policies (rejected: registries, pools and notifications are what takes longest to repeat).
- **Secrets:** never in the file, named so the importer knows what to enter again (chosen); encrypted in the file
  (rejected: one more key to manage, and a leaked file plus a guessed passphrase leaks every credential; backups
  already cover that case).
- **Applying:** through the same service functions the portal and API call, item by item in dependency order
  (chosen: every check, approval rule and audit event applies, and nothing new can bypass them); one database
  transaction across services (rejected: each service owns its transaction and its checks, and rewriting them as
  one would duplicate validation); writing rows directly (rejected: it bypasses validation).
- **Preview:** `dryRun` on the import operation, true by default (chosen: forgetting the flag changes nothing); a
  separate preview operation (rejected: two operations that must stay in step).
- **Missing items:** left alone (chosen); deleted to mirror the file (rejected: a partial file would delete
  configuration, and deletes are refused while anything refers to them).

## Decision
- **Operations.** `GET /configuration/export` answers YAML (JSON with `Accept: application/json`), administrators
  only, audited `configuration.export`. `POST /configuration/import` takes YAML or JSON (4 MiB), administrators only;
  `dryRun` defaults to true and answers the plan; `dryRun=false` needs an `Idempotency-Key`, applies the plan and
  audits `configuration.import` with the counts, besides the events each change writes itself.
- **Format.** `apiVersion: samuori.invalid/v1alpha1`, `kind: Configuration`, `metadata` (`exportedAt`,
  `samuoriVersion`, `organization`, informational) and `spec`, checked against
  `api/schemas/configuration.v1alpha1.json`; unknown fields are refused. Lists are sorted by name, maps by key, so
  two exports of the same configuration differ only in `exportedAt`.
- **Exported.** Teams referred to by notification rules (names only); sign-in settings (local sign-in, session
  timeouts); audit retention; every policy's current document (base images, build arguments, egress presets and
  destinations, frontends, publication and downloads, sensitive content, validation); the build method policy and
  provider settings; built-in recipe and shipped-runtime decisions that differ from approved, with their reasons;
  draft and approved organization runtimes; draft, approved and deprecated custom recipe versions with their
  definitions; worker pools (name, location, isolation profile, trust domain, trusted workloads only); registries;
  registry credentials as name, registry, type, purpose and repository scopes; destinations with their tag rules;
  notification channels without their secret; notification rules.
- **Never exported.** Secret values, secret providers and stored secrets, credentials' usernames and secret
  references, keys, certificates, tokens, users, service identities, role bindings, group mappings, identity
  providers, sessions, workers, enrollment and join tokens, pool approvals, projects and their sources, specs, builds,
  artifacts, publications, approvals and audit records, image sources, signing keys, cluster and federation material.
  Withdrawn runtimes and custom versions are history and are left out.
- **References.** Pools, registries, credentials and channels are referred to by name; projects as `team/project`.
  On import a name resolves to an item that exists or that the same import creates earlier. Projects are never
  created. A destination imports without allowed projects that do not exist (narrower access) and says so. A
  provider whose pool or project restriction does not resolve in full, and a notification rule whose team or project
  does not exist, are skipped, because without the restriction they would apply more widely than the file says.
- **Matching.** Items match by their natural key: policy kind, provider name, recipe ID and version (with runtime
  label), pool, registry, credential, destination, channel and team names. A notification rule has no name; one with
  the same channel, scope and events is the same rule. A matching item that differs is updated through its edit
  function with the file's settings for that item; one that is the same is unchanged.
- **Skipped, with a reason.** Credentials of any kind but anonymous, and webhook, Slack and Teams channels (they need
  a secret; an email channel needs an SMTP server); a policy change that would newly block existing specs (made on
  the Policies page, which previews the impact and asks for a reason, TM-92); a pool whose isolation profile or trust
  domain differs (changing them approves the pool again); existing credentials, which are never changed; organization
  runtimes and custom versions that left draft with other content, and status changes backwards (approvals are not
  undone); a registry, destination or channel whose kind or registry differs. Each appears under **needs attention**
  with the page where it is done.
- **Order.** Teams, sign-in, audit retention, policies, build methods, pools, providers, recipe decisions, custom
  recipes, registries, credentials, destinations, channels, rules. A failed item is reported with the service's
  message and the rest still run; items that depended on it are skipped because their reference does not resolve.
  The answer's `complete` is false when anything failed. Running the same file again is safe: what was applied is
  then unchanged.
- **Never created.** Users, role bindings, secrets, tokens and projects. A new team has no members. A new pool gets
  its creator's approval, as when an administrator creates one in the portal, and runs builds only after its workers
  enrol and pass self-tests.

## Consequences
- A second installation is configured from one reviewed file, and an administrator sees what differs before
  anything changes.
- Credentials and channel secrets are entered again by hand on the target, by design; importing the same file again
  afterwards adds what waited for them.
- Every check of the individual operations still applies, so an import can fail item by item where those checks
  refuse; it is not atomic across sections.
- New configuration kinds must be added to the export, the schema and the importer, or they are not carried.
