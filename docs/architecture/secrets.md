# Secrets Integration Architecture

Status: **Accepted as direction with conditions (OD-13; ADRs 0029 and 0031).** Delivery behavior verified by the
OD-15 spike. Providers: ADR 0015. Threats: TM-43, TM-52 to TM-59 and TM-83 to TM-84 in
[`threat-model.md`](threat-model.md). The local provider, bindings, admission readiness and lease-checked delivery
are implemented (WP16, see "Implementation"); external providers, binding requests, the canary scan and
`sensitive-content` are not.

## Secret classes

| Class | Examples | Where it may exist | Never |
|---|---|---|---|
| Controller-only | Provider credentials, registry **push** credentials, signing keys, CA keys, webhook and notification secrets | Controller memory; external providers | Sent to workers, build steps, the UI or the API |
| Build-time | SQL Server SA password, certificate passwords, TDE private key, master key, private package tokens | Provider → controller (in transit) → worker memory → BuildKit session → tmpfs secret mounts | Specs, plans, run records, generated Dockerfiles, logs, audit, cache, image layers |
| Test-time | Credentials a runtime test needs (for example a test DB password) | Same path, delivered to the runtime-test sandbox | Same as build-time |
| Registry pull | Upstream credentials for private base images | Mirror (controller-side service) | Workers or build steps (they get a scoped mirror token, ADR 0030) |
| Worker identity | Worker mTLS private key | The worker | Anywhere else (ADR 0026) |

## Model

### In the portable BuildSpec (logical declarations only)

```yaml
secrets:
  - id: sa_password
    type: text
    required: true
    usage: build
  - id: tde_key
    type: binary
    required: true
    usage: build
    mount:
      target: /certificates/SQLServerTDECert.pvk
  - id: smoke_test_password
    type: text
    required: false
    usage: test
```

- `id`: matches the Dockerfile `--mount=type=secret,id=…` or the recipe's secret reference.
- `type`: `text` or `binary`, used for UI hints and validation. Values are always delivered byte-exact.
- `required`: when `true`, a missing binding blocks admission. When `false`, an unbound secret is not mounted.
- `usage`: `build` (build steps) or `test` (runtime-test sandbox).
- `mount.target`: optional. For the `dockerfile` method the Dockerfile is authoritative. A declared target must
  match the detected one, or validation asks the user to confirm. The default is `/run/secrets/<id>`.

### Bindings (administrative records, not in the spec)

| Field | Purpose |
|---|---|
| `project_id`, `logical_id` | What is bound |
| `provider_profile_id`, `provider_reference` | OpenBao/Vault path and field, AWS Secrets Manager ARN and key, or local store key |
| `version_policy` | `latest` or a pinned version |
| `usage` | `build`, `test` or both |
| `eligible_pools` / `trust_domains` | Where it may be delivered |
| `status`, `expires_at`, `created_by`, `approved_by` | Lifecycle and audit |

Policies govern which teams may bind which provider paths (for example path prefixes per team), whether new
bindings need approval, and which pools are eligible for which classifications. Bindings can be rotated (new
version) or disabled immediately.

## Lifecycle

```mermaid
sequenceDiagram
  participant Dev as Developer
  participant C as Controller
  participant P as Secrets provider
  participant W as Worker
  participant B as BuildKit (isolated env)
  Dev->>C: submit BuildSpec (logical IDs only)
  C->>C: static Dockerfile analysis + spec union of secret IDs
  C->>C: check permission, bindings, policy, pool eligibility
  C-->>Dev: blocked with actionable message if a required binding is missing
  C->>W: lease + plan (logical IDs, binding versions; no values)
  W->>B: start solve with session secrets provider = samuori-worker
  B->>W: session request for secret "tde_key" (timing per OD-15 spike)
  W->>C: request release (lease-bound mTLS)
  C->>C: re-check lease, binding, policy
  C->>P: read value just in time
  C-->>W: value (memory only)
  W-->>B: value over the session; mounted where the Dockerfile declares
  W->>W: canary scan of final layers, then discard values
  W->>B: destroy environment (vm-per-job) or scrub
  C->>C: audit decisions (no values)
```

1. The developer submits or selects a BuildSpec.
2. The controller computes the required IDs: spec declarations united with secret mounts detected statically in the
   Dockerfile (`build-execution.md`). Undeclared detected IDs are shown for confirmation. IDs that cannot be
   determined statically (for example `id=$VAR`) are reported as ambiguous.
3. Admission checks project permission, binding status and usage, policy and pool eligibility, all before
   scheduling.
4. Values are released only when BuildKit requests them through the attempt's session, and only for IDs the plan
   lists. A request for an ID that is not in the plan fails as "unauthorized secret mount". Observed (OD-15):
   BuildKit requests a secret each time a `RUN` that mounts it executes and never on a cache hit. The session is a
   real boundary only on a daemon running one build at a time with a scoped cache, and values persist in daemon
   memory after use. Pending decisions OD-16 to OD-18 (`docs/spikes/buildkit-secrets.md`).
5. BuildKit secret mounts are tmpfs and excluded from layers and cache. Only steps that declare a mount see the file.
6. Nothing is intentionally persisted. The canary scan and sensitive-output checks catch accidental persistence.
7. On completion, failure, cancellation or timeout, the controller stops serving the attempt's secrets, the worker
   drops values from memory, and the environment is destroyed or scrubbed. The outcome is recorded.
8. Every decision is audited without values.

## Rules for values

- Never in: BuildSpec, BuildRun or attempt records, plans, generated Dockerfiles, audit events, API responses,
  previews, diagnostics, error messages or logs.
- Never passed as build arguments or environment variables by Samuori. `RUN --mount=type=secret,env=NAME` is supported
  only with a warning, because environment-exposed values leak more easily (process listings, crash dumps).
- Log streams are masked for every delivered value and its common encodings (base64, hex). Masking is defense in
  depth only.
- Build arguments are for non-secret values. Names that look secret (`*PASSWORD*`, `*TOKEN*`, `*SECRET*`,
  `*_KEY`) trigger a warning that requires confirmation. Argument values appear in image history.

## Sensitive build outputs

Build scripts can write material derived from secrets into the image. The `sensitive-content` check (ADR 0031)
runs on every final image as part of its ValidationRun.

| Rule class | Default outcome | Examples |
|---|---|---|
| Delivered secret bytes found in final layers (canary; supplementary only) | `block` | Any delivered value, byte-exact. Cannot detect transformed, encoded, encrypted or derived values |
| Private-key material | `block` | PEM private keys, `*.pvk`, `*.key` with key content, PKCS#12 (`*.pfx`, `*.p12`), keystores |
| Known database secret directories | `require-exception` | `/var/opt/mssql/secrets`, `/var/lib/postgresql/data/pg_hba.conf` and server keys, MySQL keyrings |
| Database data directories | `warn` | `/var/opt/mssql/data`, `/var/lib/postgresql/data`, `/var/lib/mysql` |
| Credential files | `require-exception` | `.env`, `.aws/credentials`, `.docker/config.json`, kubeconfigs, `id_*` SSH keys |
| Secret-like values in image config or history | `require-exception` | `ENV DB_PASSWORD=…`, secret-looking build arguments |

- Administrators can add rules and change outcomes per organization, team or project, within org policy.
- **Exceptions** are scoped (project, path patterns, destination class), justified in writing, time-limited and
  audited. An image that
  passes through an exception is classified `restricted` and may be published only to destinations marked as
  accepting restricted images. Promotion re-evaluates the classification.
- Specs may declare expected sensitive outputs, with a reason, so the run page explains them. A declaration does
  not grant an exception.
- Findings show rule, path, layer, size and logical secret ID; never contents or values. Scanners run within CPU,
  memory, time and size limits and never log matched content.
- A mandatory scan that fails, times out or is incomplete blocks publication (verdict `incomplete`).
- The controller enforces `restricted` classification at publication on every channel.
- Samuori never deletes or modifies application files to remediate.

### Worked example: SQL Server image

A Dockerfile mounts `sa_password`, `certificates_password`, `master_key`, `tde_cert` and `tde_key`, starts SQL
Server, restores databases, and copies `/var/opt/mssql/data` and `/var/opt/mssql/secrets` into the final stage.

1. Static analysis finds five secret IDs with their targets. The secrets panel shows their binding status.
2. The build runs with each value mounted only in the step that declares it.
3. `sensitive-content` reports:
   - `/var/opt/mssql/secrets` → `require-exception`;
   - `/var/opt/mssql/data` → `warn`;
   - the canary → whether any delivered value appears verbatim.
4. Without an approved exception the image is `blocked` for publication. With one it is classified `restricted`
   and can go only to restricted-allowed destinations.

## Worker compromise

An authorized worker must receive the build-time secrets for the builds it runs. If that worker or its build
environment is compromised during a build, those secrets are exposed. Samuori reduces exposure:
- one build per VM (`vm-per-job`);
- release only through the attempt's session, only for IDs in the plan;
- refusal after lease end or revocation;
- bindings scoped to eligible pools;
- short-lived provider access on the controller.

Samuori does not claim confidentiality in that case. Administrators can restrict high-value bindings to dedicated pools.

## Implementation (WP16)

| Part | Where | Notes |
|---|---|---|
| Local encrypted provider | `internal/secrets` | One built-in provider record per organization. Values are AES-256-GCM encrypted with a 32-byte key in `<data dir>/keys/secrets.key` (created on first start; refused if readable by group or others). The secret ID and version are authenticated data, so ciphertexts cannot be moved between rows. Versions are immutable. The key sits on the same host as the database: encryption protects database copies and backups, not a compromised controller. Key rotation is not implemented |
| Values in the API | `POST /secret-providers/{id}/local-secrets` | Write-only (text, or base64 for binary, at most 64 KiB). Storing an existing name creates a new version. Listing returns metadata only |
| Bindings | `POST /projects/{id}/secret-bindings`, `POST /secret-bindings/{id}/actions` | Administrators only. One active binding per project and logical ID; binding again replaces it. Usage `build`, `test` or both; `latest` or a pinned version; optional eligible pools and expiry; a reason is required. `rotate` gives the binding a new version, `disable` stops it at once. Every change is audited without values. Rotating a local secret bumps every `latest` binding to it |
| Admission | `internal/admission` | Readiness per declared secret: `ready`, `missing` (blocks when required, with the action to take), `unbound` (optional, not mounted). Bindings of other projects are never considered |
| Plan | `buildplan.PlanSecret` | Logical ID, target, binding ID and binding version. Never values or provider references |
| Scheduling | `internal/builds` | A binding's eligible pools restrict where the run may be assigned |
| Release | `ReleaseSecret` (worker protocol) | Only for the worker holding the current, unexpired lease of a preparing or building attempt, only for IDs the plan binds, and only while the binding is active at the admitted version. Optional unbound IDs get an explicit "unbound" answer; everything else is a denial with a reason. Every decision is audited |
| Railpack | `rootlesshost` (ADR 0047) | Railpack steps read secrets from environment variables, so a build secret reaches them as `RailpackSecretEnv(id)` (`npm-token` → `NPM_TOKEN`). The plan maps each variable to its logical ID; the worker serves BuildKit's request for the variable by releasing the logical ID, declares only bound secrets in the Railpack plan, and refuses any other name. Values never reach `railpack prepare`. File mounts are refused for this method |
| Buildpacks | `rootlesshost` (ADR 0047) | A build secret reaches buildpacks as a CNB service binding file `/platform/bindings/<name>/<key>`: a BuildKit secret mount by logical ID on the lifecycle step only (mode 0400, owned by the builder's CNB user, `required=true`). The binding's `type` file is non-secret and comes from the BuildSpec. Unbound optional secrets get no binding. Values never enter the plan, the platform directory, the Dockerfile or the log; the canary scan also looks for the build's egress-proxy credential, which the lifecycle hands to every buildpack. A buildpack may still copy a binding into a layer, which the canary blocks (ADR 0031). `TestBuildpacksDockerfile`, `TestBuildpacksRequest`, `TestBuildpacksLayout`; real run in `../spikes/buildpacks-builds.md` |
| Delivery | `internal/adapters/build/buildkit`, `rootlesshost` | `samuori-worker` is the BuildKit client (Go client library) and serves secrets through the session store when BuildKit asks: once per `RUN` that mounts the ID. IDs outside the plan fail the step; values stay in memory. Released values and their base64 and hex forms are masked in the run log (defense in depth only). Daemons run with build history off, because history keeps raw step output |

Evidence: unit and API tests (store, bindings, readiness, release checks, masking) and real builds in the Linux dev
VM (`make test-buildkit`, removed with the tests, ADR 0039; results are history): byte-exact text and binary values at custom targets, no value in image layers, config,
history or daemon state, failed and cancelled builds leave nothing, revocation between steps fails the build, and
concurrent builds with the same steps and different values each get their own result.

## Grants

Owner decision, 2026-10-21. Administrators decide which keys a project may use; the project's developers and
maintainers decide which key each spec secret ID uses.

- **Grant:** an administrator grants a project one key: a local secret, or a secret from an external store that is
  on the [allowed list](#allowed-secrets) (the whole entry, or one key of an allowed whole secret), with its usage,
  version policy and eligible pools. The reference is checked to resolve, as for a binding.
  Grants are audited (`secret.grant`, `secret.grant.revoke`).
- **Bind:** a developer or maintainer of the project picks a granted key for a spec secret ID, in the image wizard
  (`POST /projects/{id}/secret-grant-bindings`). The binding takes the grant's reference, usage, version policy and
  pools; the caller chooses nothing else. No further approval: the grant is the approval. Only active grants of the
  same project can be bound, and the server checks the caller's role on that project.
- **Key sets:** for OpenBao or Vault, a grant may name a whole KV v2 secret (a path without `#key`). Samuori reads it
  once to record its **key names** and discards the values. Developers choose a key from the set for each spec
  secret ID; the binding then points at `path#key`, so each build secret receives exactly one value and log masking
  and the canary work per key. A direct binding of a whole set is refused. **Refresh keys** rereads the names and
  disables bindings to keys that no longer exist. The wizard's **Use matching keys** binds every secret ID that has
  the same name as a key in a granted set.
- **Visibility:** members see key names, the store's name and which IDs use each key. Store references, values and
  raw bindings stay with administrators.
- **Revoke:** revoking a grant disables every binding made from it at once, so admission refuses it and running
  builds can no longer receive it.
- **Edit** (ADR 0064, `PUT /projects/{id}/secret-grants/{grantId}`, administrators; Project access > **Edit**): the
  name, usage, version policy and eligible pools change in place; the key does not. The checks of creation apply
  and a reason is required. Bindings made from the grant follow the new scope. One the new usage, pools or key
  names no longer cover is **disabled** at once with the reason `grant narrowed: ...` (audited `secret.unbind`);
  the others stay active at their version, so a widening interrupts nobody. A changed version policy gives the
  kept bindings a new version, because they would now deliver a different value than running builds were admitted
  with. Direct bindings and other grants are never touched. Audited `secret.grant.update` with before and after;
  the portal names the IDs a narrowing would disable before saving. `TestUpdateGrant`.
- **Stopping use:** unbinding stays administrator-only (**Disable** under Project access). Developers and maintainers
  stop using a key by binding the ID to another key or removing the secret from their spec. **Rotate** under
  Project access gives a binding a new version, for example after its value changed in an external store.
- Administrators can still bind directly (`POST /projects/{id}/secret-bindings`); developers cannot. "Ask for
  binding" remains for keys nobody granted yet.
- In the portal: Security > Secrets has the tabs Stored secrets, External stores and Project access. Each stored secret has a **New version** action: it stores the next version under the same name and type; grants on "latest" use it from the next build, pinned grants keep their version. **Delete** destroys every version of a stored secret and is audited (`secret.delete`). It is refused, with each use listed, while a project has an active grant or binding of it, or while Samuori uses it for its own credentials; see [Deleting secrets](#deleting-secrets).

## Deleting secrets

`DELETE /secret-providers/{id}/local-secrets/{secretId}` (administrators; **Delete** on Stored secrets) destroys
every version and is audited (`secret.delete`). It is refused, listing each use, while:

- a project has an active grant or binding of the secret, or
- any Samuori record refers to it: registry credentials, signing keys, identity providers, the image mirror, notification
  channels, pipelines (token and webhook) or external store credentials. These count whatever their status, so
  history such as a retired signing key keeps its key.

**Controller-only secrets** are kept for the controller (registry credentials and the secrets Samuori generates for
itself). They can never be granted or bound to builds, whether or not anything still uses them. When the credentials
that used one are deleted or switched to another secret, the secret stays controller-only; it is not released to
builds. Owner decision (2026-10-26, provisional): such a **leftover** secret, one no current or history row refers
to, may be deleted by an organization administrator like any other, and the audit records that it was a
controller-only secret Samuori no longer used. The listing returns `controllerOnly` and `samuoriUses` (what Samuori keeps it
for), and Stored secrets shows **No longer used by Samuori** for a leftover. A controller-only secret that something
still refers to is refused as before. The check and the delete share one transaction. Known gap: a few flows
(pipelines, notification channels) store their generated secret just before inserting the row that refers to it, in
separate transactions; an administrator deleting that secret in between would leave the new record without its
secret, which then fails when used. Evidence: `TestDeleteSecret` (leftover controller-only secret),
`TestRegistryEditAndDelete` (leftover controller-only secrets).

## Allowed secrets

Owner decision, 2026-10-25: a store connection allows nothing by itself. Administrators list the secrets of external
stores that may be granted (Secrets > **Allowed secrets**, `GET`/`POST /allowed-secrets`, `DELETE
/allowed-secrets/{id}`): a whole OpenBao or Vault secret (projects choose its keys) or one key, each with its own KV
v2 mount (`mount:path` or `mount:path#key`, so one store serves several mounts; a reference without a mount uses the
store's default, `secret` unless set through the API), or an AWS secret
or one JSON field, each with a label and a note. Adding an entry reads it once to prove it exists (a whole secret
records its key names) and discards the values. Grants of external secrets must come from the list: by
`allowedSecretId`, or by a reference the list covers. Removing an entry stops new grants; grants already made stay
until revoked. **Edit** (`PUT /allowed-secrets/{id}`) changes only the label and note; grants already made keep
their own names, and the store and reference cannot change. A store is not deleted while entries name it. Every
change is audited (`secret.allow`, `secret.allow.update` with before and after, `secret.allow.remove`). The older per-store **allowed prefixes** still apply where set but are no longer edited in
the portal. Migration 00058, `TestAllowedSecrets`.

## External providers (WP27)

ADR 0015 lists the provider types. Administrators add them under **Secrets → External secret stores**
(`POST /secret-providers`). A store's credential is written once and kept as a **controller-only** local secret
(token, AppRole secret ID, or AWS access key). It is never returned, never bound to a build, and never sent to a
worker.

| Type | Connection | Authentication | Reference | Pinned version |
|---|---|---|---|---|
| `openbao` | `https` address (plain `http` only on loopback), KV v2 mount, optional namespace and CA | token, or AppRole (role ID in settings, secret ID as credential; client tokens cached until shortly before their lease ends). The secret ID is optional for a role created with `bind_secret_id=false`, which Vault then must bind to the controllers' addresses with `token_bound_cidrs` (`TestAppRoleLogin`) | `path/to/secret#field` | KV version number |
| `vault-compat` | Same API as OpenBao; Vault is BUSL-licensed and operator-provided, never bundled | Same | Same | Same |
| `aws-secrets-manager` | region, optional endpoint (VPC endpoint or emulator) | the controller's workload identity (default chain); assume a role with an optional external ID; or a stored access key, reported as long-lived | name or ARN, with `#key` for a field of a JSON secret | version ID or staging label (`AWSPREVIOUS`) |

- **Just in time.** A value is read from the store only when a build holding a lease asks for it (`ReleaseSecret`),
  after the same lease, plan, version and usage checks as local values. Values are never cached or written to disk.
- **Base64 values.** Stores often hold certificates and keys as base64 text. A binding to an external store may set
  `encoding: base64` (developers choose it per key in the wizard; administrators on a direct binding). Binding reads
  the value once and refuses it if it does not decode. On release the value is decoded (padded or not, line breaks
  ignored), so the build, log masking and the canary all see the decoded bytes; a value that stops decoding is not
  released. The local store keeps binary values as bytes and refuses the option. Migration 00056,
  `TestBase64Bindings`.
- **Binding.** Binding an external reference reads it once to prove it resolves, then discards the value. Optional
  **allowed prefixes** on a store limit which references may be bound at all.
- **References** are validated per type. KV paths may not contain `.` or `..` segments, and mounts are checked the
  same way.
- **The OpenBao client** is plain HTTP with no SDK. It never follows redirects, because Go would forward
  `X-Vault-Token` to the redirect target.
- **View, edit and delete** (`PUT` and `DELETE /secret-providers/{id}`, Secrets > External stores > **View or
  edit** and **Delete**). Every setting except the credential is shown; the type cannot change. An empty credential
  keeps the stored one, but changing the address, AWS endpoint, sign-in method or AppRole role ID needs it entered
  again, so a stored credential is never sent somewhere new. Saving clears the last test. Delete also deletes the
  stored credential; it is refused while grants, bindings or signing keys use the store, or ever did, because their
  history names it. Both are audited (`secret-provider.update`, `secret-provider.delete`). `TestUpdateAndDeleteProvider`.
- **Test** (`POST /secret-providers/{id}/test`) proves authentication without reading a secret: OpenBao token
  lookup, or AWS `GetCallerIdentity`. It records the result and the identity used.

The same connections serve **signing keys held in Transit or KMS** (`signing.md`). External stores are not used for
controller-only credentials; registry and identity-provider credentials stay in the local store.

Evidence:
- **Unit and API tests:** provider validation, including a `../sys` mount that the first draft accepted; credentials
  encrypted and controller-only; allowed prefixes; dot segments; unreadable references refused at bind time; latest
  and pinned reads; replaced and other-version bindings refused; the token never following a redirect; and API role
  checks.
- **Live, in the dev VM** (`make test-buildkit`):
  - **OpenBao 2.7.1** (`provision-openbao.sh`, checksum taken from the release's Sigstore-verified
    `checksums.txt`): token and AppRole sign-in, latest and pinned KV versions, a path outside the AppRole policy
    denied, and a wrong secret ID refused.
  - **AWS Secrets Manager**, emulated by moto 5.2.3 (`provision-aws-emulator.sh`; no account, no cost): JSON keys,
    binary secrets, `AWSPREVIOUS`, a missing secret, the caller identity, and assume-role with an external ID.
- **Not tested live:** Vault itself (licensing; the API is the same as OpenBao's), and a real AWS account.

Not implemented: developer binding requests (approvals, WP22), test-time delivery (WP20), store key rotation, and
delivery inside `vm-per-job` (OD-12b).
