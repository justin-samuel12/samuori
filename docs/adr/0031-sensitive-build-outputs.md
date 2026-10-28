# 0031. Sensitive build outputs

Status: **Accepted as direction, with conditions** (2026-10-19, owner decision OD-13). Design: [`../architecture/secrets.md`](../architecture/secrets.md#sensitive-build-outputs).

## Context
Secret mounts keep the supplied files out of image layers. They do not stop build scripts from writing material
derived from those secrets into the image. For example, a SQL Server build that restores databases and installs
TDE certificates, then copies `/var/opt/mssql/data` and `/var/opt/mssql/secrets` into the final stage. Using
`RUN --mount=type=secret` does not make an image safe.

## Decision
- Samuori distinguishes **build-time secrets** (inputs) from **sensitive build outputs** (files produced with them).
- Every final image goes through a `sensitive-content` check in its ValidationRun:
  - **path rules:** a curated, versioned list (database data and secret directories, key and keystore extensions,
    credential files), extended by administrators;
  - **content detectors:** private keys, PKCS#12 stores and known credential formats, using a pluggable secret
    scanner;
  - **build-secret canary (supplementary only):** the worker checks the final layers for the exact bytes of every
    secret delivered to the build, before the values are discarded. It cannot detect transformed, encoded,
    encrypted or derived values; path rules and detectors remain the primary controls;
  - **metadata:** image config and history, for secret-like build arguments and environment values.
- **Policy outcomes** per rule class are `warn`, `require-exception` or `block`. Defaults: `block` for a delivered
  secret's bytes or private-key material in final layers; `require-exception` for known database secret
  directories; `warn` for database data directories.
- **Exceptions** are approved by administrators. Each is scoped to a project, path patterns and destination class,
  time-limited and audited. An image that passes through an exception carries a `restricted` sensitivity
  classification. It may be published only to destinations marked as allowed for restricted images, and promotion
  re-evaluates it.
- **Scanner safety.** Scanners and the canary never write secret values or matched content to logs, diagnostics,
  findings or stored metadata. Findings carry rule, path, layer, size and the logical secret ID only. Scans run
  inside CPU, memory, time and size limits.
- **Fail closed.** A mandatory scan that fails, times out or is incomplete makes the ValidationRun verdict
  `incomplete`, which blocks publication. It never passes silently.
- **Controller enforcement.** The controller enforces the `restricted` classification at publication
  (`gate_check`), on every channel including the direct API.
- Exceptions require a written justification as well as scope and expiry.
- Intentionally sensitive artifacts, such as database images, go through the same authorization, approval and
  publication controls; an exception never bypasses them.
- Samuori **never deletes or rewrites** application files to "fix" an image. Findings show path, size, rule and layer;
  never contents.
- A spec can **declare expected sensitive outputs** with a reason, so the run page explains them up front. A
  declaration does not replace the exception.

### Implementation (WP20, 2026-10-19)
Implemented as described in `docs/architecture/validation.md`: path rules, content detectors, metadata checks and the
worker canary, with fail-closed verdicts.

### Implementation (WP25, 2026-10-19)
Exceptions are scoped to a project, image path patterns and optional classes, need a justification, expire within 90
days and are decided by an administrator other than the requester. Every step is audited. A run that passes only
through an exception is `restricted`, and the publication `classification` gate refuses restricted images at
destinations that do not accept them, including through promotion, which revalidates first
(`docs/architecture/validation.md#output-exceptions-adr-0031`).

## Consequences
- Specialized database images remain possible through an explicit, auditable workflow with restricted distribution.
- Path rules and detectors need upkeep and will have false positives. Outcomes are explained in plain language.
