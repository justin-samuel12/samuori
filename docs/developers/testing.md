# Testing

Status: **focused tests only** (ADR 0039 as amended, 2026-10-20). The broad suites were removed; focused regression
tests for critical security boundaries and previously found vulnerabilities were restored.

- `make test` runs them on the host (part of `make ci`); `make test-vm` runs them in the Linux dev VM, where the
  seccomp and sandbox tests apply. The rows they cover are marked in `security-test-catalog.md`.
- `make test-acceptance` runs the browser journey (setup, upload, discovery, editors, a real build, validation and
  publication to the local registry) against the all-in-one install in `samuori-clean` (`make aio-clean`).
- Practical checks (installs, HA scenarios) are recorded as evidence with the commit they ran at.
- `make ci` passing is not full coverage: it says nothing about rows not marked restored.
