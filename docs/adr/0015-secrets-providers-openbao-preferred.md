# 0015. Secrets providers: OpenBao preferred, Vault optional compatibility

Status: Accepted (2026-10-19) — owner decision

## Context
D10 lists "Vault/OpenBao optional". HashiCorp Vault changed to BUSL-1.1 on 2023-08-11 (v1.15.0+); it is not an
OSI-approved license. OpenBao is the community fork under MPL-2.0 (Linux Foundation / OpenSSF).

## Decision
- Secrets adapter interface with providers: **local encrypted** (default, no external service), **OpenBao**
  (preferred external), **AWS Secrets Manager**, and a **Vault compatibility adapter** that uses the shared API
  surface. Samuori never bundles or redistributes Vault; using it is the operator's licensing decision.
- PKI for worker identities: internal CA by default; OpenBao PKI optional; Vault PKI via the compatibility adapter.
- OpenBao/Vault are reached over their HTTP APIs as separate services. We do not vendor or modify MPL-2.0 files; if
  that changes, MPL file-level obligations apply.
- Specs and logs only ever hold references; values are released just in time, scoped to one job.

## Implementation (WP27, 2026-10-20)
- OpenBao and the Vault compatibility type read KV v2 over HTTP with token or AppRole authentication; no SDK and no
  MPL-2.0 files are vendored. AWS Secrets Manager uses aws-sdk-go-v2, preferring workload identity and assume-role.
- Provider credentials are controller-only local secrets. Values are read just in time per lease.
- Transit and KMS also hold image signing keys (ADR 0021).
- OpenBao PKI for worker identities is not implemented; the internal CA remains.
- Conformance runs against an OpenBao dev server and a local AWS API emulator (`secrets.md`).

## Consequences
The admin UI labels Vault "compatibility (BUSL-licensed, operator-provided)". Conformance tests run against OpenBao;
Vault compatibility is tested where a license-compatible test setup is available.
