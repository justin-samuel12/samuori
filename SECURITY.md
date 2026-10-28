# Security Policy

## Supported versions

Samuori is pre-release. No version is supported for production use, and fixes land on `main` only.

## Reporting a vulnerability

Please report vulnerabilities privately, never in a public issue, pull request or discussion.

- Preferred: GitHub private vulnerability reporting. Open the repository's **Security** tab and choose
  **Report a vulnerability**.
- Or email the maintainer, Justin Samuel, at jsamuel12@gmail.com.

Include what you found, the affected version or commit, steps to reproduce, and the impact you expect. Samuori is
maintained by one person on a best-effort basis, so there is no guaranteed response time. You will get an
acknowledgement, and we can agree on a disclosure date once the issue is understood.

## Scope

In scope: the Samuori server, worker, CLI and web UI, the install scripts under `deploy/`, and the security
boundaries described in `docs/architecture/security.md`. Vulnerabilities in the upstream tools Samuori runs (for
example BuildKit, Buildah or Trivy) belong with those projects; tell us too if Samuori's use of them makes the issue
worse.

## Current state

- The threat model is in `docs/architecture/threat-model.md`, and the planned verification in
  `docs/developers/security-test-catalog.md`.
- Only some mitigations are verified by current tests. The broad test suites were removed and focused regression
  tests for critical boundaries were restored (ADR 0039 as amended); the catalog marks which rows they cover.
  Other results in the catalog come from earlier commits.
- Untrusted source should build only on pools with an approved and verified isolation profile. The `vm-per-job`
  provider has not been validated on a real KVM host (ADR 0038).
