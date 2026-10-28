# Release Readiness Matrix

Status as of 2026-10-20. A status record, not a readiness claim. Samuori can be installed and evaluated hands-on in a
controlled environment on dedicated Linux VMs. It is **not** production-ready: most security controls have only historical
test evidence (ADR 0039 as amended).

Columns: **Verified** means exercised at the current code or by a current test (on or after 2026-10-20).
**History** means it passed at an earlier commit, before most tests were removed. **Unverified** means
implemented but never exercised. **Blocked** names the missing environment or decision.

| Area | Verified now | History | Unverified / blocked |
|---|---|---|---|
| All-in-one install on a clean host | Fresh Ubuntu 26.04 arm64 VM: install, setup, browser journey 5/5 (rerun 2026-10-21 on the redesigned UI, with automated accessibility checks) (upload, discovery, editors, build, validation, publish to local registry), restart keeps state | Ubuntu, Debian 13, Compose, emulated amd64 | Native amd64 hardware |
| Controller cluster (3 controllers, PostgreSQL, NATS JetStream, S3, load balancer) | Six local VMs, installed with the installer: init, token join, two workers; failover, leader loss, worker loss, quorum loss, partition, rolling upgrade (`operators/high-availability.md#evidence`) | k3s rehearsal | Physical or cloud hosts; RDS; AWS ALB; publication from the cluster |
| Controller join and installer roles | Exercised as above; focused tests SEC-HA-03 | — | — |
| Installer with an external NATS cluster (`--nats-url`) | Focused test SEC-HA-03 (bundle, join, generated configuration) | — | Not exercised against a running external NATS cluster |
| Workers: enrollment, self-test, strict egress | Two remote workers enrolled and passed self-tests | Full SEC-ISO and SEC-NET suites | Cloud metadata isolation (SEC-NET-02) |
| Controller and remote worker on separate hosts | `make test-acceptance-two-node` on two fresh VMs: token enrollment, self-test, a build on the remote worker, validation and publication, 5/5 (2026-10-21) | — | Separate physical or cloud hosts |
| `vm-per-job` (Cloud Hypervisor) | Host side against a fake guest (SEC-ISO-11) | — | **Blocked:** needs a KVM host where guests run (OD-12b) |
| Discovery sandbox and seccomp | Focused tests on Linux (SEC-DSC-01..03, 05) | — | — |
| Authentication, RBAC, uploads, webhooks, mirror tokens | Focused tests restored (`security-test-catalog.md`) | Wider suites | Rows not marked restored |
| ECR | — | Against an emulator | **Blocked:** a live AWS account (INT-REG-03) |
| Secrets: local, OpenBao, AWS Secrets Manager | Local store in the journey | OpenBao, emulated AWS | Live AWS Secrets Manager |
| Vulnerability scanning | `make vuln` for Go dependencies | Grype and Trivy adapters | **Blocked:** OD-10 (no default scanner) |
| Identity providers | Local sign-in | OIDC and LDAP against test servers | Commercial OIDC, Active Directory |
| Notifications and Git hosts | — | Local | Slack, Teams, Git host deliveries |
| License policy, SBOM, notices, signed release artifacts | `make ci` (license policy), dry runs | — | Samuori's own `LICENSE` and `NOTICE` |
| Windows execution | — | — | **Blocked:** OD-7b |

## Blockers for a wider pilot

1. AWS: not yet exercised; follow the checklist in `operators/aws.md`.
2. Isolation: only `rootless-dedicated-vm` is approved for uploaded source; `vm-per-job` is unvalidated.
3. Verification: most catalog rows have only historical evidence.
4. Open decisions: OD-10 (scanner), OD-12b (KVM host); the audit findings await owner review (go-jose is done).
