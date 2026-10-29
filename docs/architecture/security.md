# Security Requirements and Threat Model

Status: **Authoritative** for requirements. The threat model (assets, actors, data flows, per-boundary threats,
mitigations, test IDs and residual risks) is in [`threat-model.md`](threat-model.md).

## Threat actors

Malicious/untrusted source uploaders; compromised workers; token theft; cross-project escalation; CI credential
misuse; malicious dependencies; registry tampering; message replay; resource exhaustion.

## Network zones and trust boundaries

Public UI/API · Admin operations · Messaging · Artifact stores · Builder workloads · Credential issuers · OCI
registries. Each boundary is documented with its authentication, authorization and mitigating tests.

## Must be disallowed (each needs a negative test)

- Public (unauthenticated) worker registration; non-expiring or reusable enrollment secrets.
- SSRF, including to cloud metadata endpoints; unvalidated URL fetches; untrusted registry URLs without network policy.
- Shell injection via spec arguments.
- Archive escapes; uploads of device files, symlinks or hardlinks escaping the root; special files.
- Secret literals in specs or logs.
- Privilege escalation inside runtime-test sandboxes.
- Cross-team access to sources, secrets, logs, caches, artifacts, repositories.
- Build cache poisoning.
- Host Docker socket in a build container; uncontrolled privileged execution; arbitrary host mounts; unrestricted
  secret access.

## Authorization

RBAC is evaluated by the controller API **and enforced again** on task dispatch, artifact access, worker-scoped
credentials and publication. Approvals and exceptions are immutable audit objects with actor, reason, expiration
and scope. A pending request may be withdrawn by its requester or a project maintainer before anyone decides it;
the withdrawal is itself recorded and audited, and never changes a decision (ADR 0062).

## Upload handling

Normalize UTF-8 and Unicode paths; reject traversal, symlink/hardlink escapes, special files, deep/recursive archive
bombs, path overlaps (including case-folding collisions) and flag suspicious executables. Run malware and secret
preflight checks where practical; never claim detection is exhaustive.

## Operator logos

An installation may show its own logo in the top bar and on the sign-in page (ADR 0072). Uploaded images are
untrusted. PNG and SVG only, bounded at 512 KB, with the format confirmed server-side (PNG decoded, SVG parsed).
Every SVG is sanitized before it is stored or served, through an allowlist of safe elements and attributes rather
than a denylist: scripts, `on*` event handlers, `<foreignObject>`, remote references (`href`/`xlink:href`, `<image>`,
`url()` to anything but a same-document fragment), DOCTYPE and entities are dropped; safe geometry and local gradient
references survive. The logo blobs are served with their exact `Content-Type`, `X-Content-Type-Options: nosniff` and
a restrictive `Content-Security-Policy` (`default-src 'none'; sandbox`) so an image opened directly cannot execute or
fetch. The two logo reads are unauthenticated because the sign-in page precedes authentication; they return only the
image bytes, carry no other data, take no identifier (one brand per installation, ADR 0071) and 404 when none is set.
Setting, uploading and clearing are administrators only, enforced server-side. Branding never removes the Samuori
attribution, the `NOTICE` or the licence, and the About dialog still identifies Samuori. See SEC-BRAND-01 and TM-134.

## Outbound requests

No URL fetch can reach metadata services or private endpoints unless allowlisted. Enforce redirect limits and
DNS/IP-rebinding controls (resolve-then-connect with the validated IP).

## Build and test isolation

- Default-deny egress for untrusted builds; opt-in allowlists for dependency endpoints and internal services.
- No unrestricted metadata-service access from any build/test sandbox.
- No build of uploaded source runs on a pool whose isolation profile the owner has not explicitly approved
  (ADR 0013, OD-4). The scheduler treats such pools as ineligible.
- Ephemeral per-build environments; CPU/memory/disk/network/time limits; rootless where supported; VM-based
  profiles for stronger isolation; optional dedicated pools (D24). Isolation profiles are evaluated in the M0
  BuildKit spike (ADR 0013).
- Kubernetes conversion tools (Kompose) get the same eligibility as untrusted builds, run with no network, no
  secrets and no registry credentials, only at the approved checksum; their output is untrusted until Samuori
  validates it (ADR 0048, `kubernetes-conversion.md`).

## Secrets

Design: [`secrets.md`](secrets.md) (build secrets, bindings, sensitive outputs) and [`registry-auth.md`](registry-auth.md).

- Never in BuildSpec or ordinary build args. Use BuildKit secret mounts / SSH forwarding with tight scope; disable
  credential caching.
- Just-in-time, job-scoped release by the secrets adapter (ADR 0015).
- Mask known secrets and structured sensitive fields in streamed and stored logs. Redaction is defense in depth,
  not sufficient alone.
- Distinguish user permissions, control-plane roles and scoped worker permissions. Prefer workload identity /
  temporary STS credentials.
- Signing keys and PKI material in dedicated providers where possible.

## Supply chain

Scan results tie to image digest plus scanner and vulnerability-DB version/time. Policy is re-evaluated on
promotion. Build-plan integrity relies on immutability, digests, lease-bound authorization and mTLS delivery; plan signing is deferred pending the analysis in ADR 0014. Images are signed with Cosign using administrator-managed keys (ADR 0021).

## Required bypass tests

Direct API calls bypassing the UI; malformed webhooks; expired approvals; mTLS impersonation; concurrent tag changes;
stale fenced results; replayed messages. Build-plan integrity: plan requested by a worker without the current
lease; plan digest mismatch; plan content in a NATS payload; stale plan replayed after reassignment (ADR 0014).
