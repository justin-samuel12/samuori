# 0039. Test code removed by owner directive

Status: Accepted (2026-10-20, owner directive); **amended the same day** (see "Amendment"). It supersedes the testing requirements of `CLAUDE.md`, ADR 0011's
test targets and `docs/developers/testing.md` until the owner reverses it.

## Context
During M12 the owner directed that all test suites be removed from the repository. Asked to confirm the scope, the
owner chose to delete all test code: every `*_test.go`, the UI unit and browser tests, the test fixtures, the test
support packages, the test-only make targets and the HA rehearsal scripts. The owner was told the consequences
before confirming.

## Decision
- Removed: 132 Go test files, 27 UI test files (unit tests, Playwright specs, fixtures and configs), 66 `testdata` fixtures, `internal/testsupport` (7 files); 237 files in all, the
  rehearsal scenario scripts (`tools/k3s` failover, partition and setup; the uncommitted `tools/ha`), the browser
  spike's test spec, and the targets `test`, `test-integration`, `test-vm`, `test-buildkit`, `test-kvm`,
  `test-*-amd64`, `test-e2e`, `test-acceptance*` and `schema-check`. The test frameworks left the UI's
  dependencies.
- `make ci` now checks generated code, formatting, `go vet`, the build, the API contract, file naming, UI type
  checking, lint, formatting and build, and the license policy. It runs no tests.
- Kept: product code, including security controls written to satisfy tests, and the spike experiment scripts
  under `tools/spikes`.

## Consequences
- **No mitigation is verified.** Every security catalog row is "not implemented: tests removed". Results recorded
  in the docs (catalog, roadmap, operator evidence sections, spike reports) describe runs at commits
  before this one. They are history and can no longer be reproduced from the tree.
- Under `docs/product/acceptance-criteria.md`, the exit checklist item on security negative tests is unmet. No
  milestone acceptance that depended on tests can be claimed. Samuori is not ready for a pilot handling untrusted
  source on the strength of tests.
- Restoring the tests means reverting this change (`git revert`); the removed files remain in history.

## Amendment (2026-10-20, pilot-readiness directive)

The owner's pilot-readiness directive asks for focused negative tests for critical security boundaries and
previously found vulnerabilities, and for practical integration and browser checks, without rebuilding the old
suites. Restored from history: the discovery sandbox and seccomp tests, upload, authentication, RBAC and repository
request tests, the mirror-token tests, the Buildah link-safety tests, webhook and blob tests, worker reconciliation
and boundary tests, the `vm-per-job` fake-guest and protocol tests, and the license-policy test. `make test` (and
`make test-vm` for the Linux-only ones) runs them, and `make ci` includes `make test`. New tests are added only where
they verify a change to a security boundary or a practical workflow. `make ci` passing is not full coverage.
