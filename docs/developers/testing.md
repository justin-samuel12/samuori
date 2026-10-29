# Testing

Status: **focused tests only** (ADR 0039 as amended, 2026-10-20). The broad suites were removed; focused regression
tests for critical security boundaries and previously found vulnerabilities were restored.

- `make test` runs them on the host (part of `make ci`); `make test-vm` runs them in the Linux dev VM, where the
  seccomp and sandbox tests apply. The rows they cover are marked in `security-test-catalog.md`.
- `make test-acceptance` runs the browser journey (setup, upload, discovery, editors, a real build, validation and
  publication to the local registry) against the all-in-one install in `samuori-clean` (`make aio-clean`).
- Practical checks (installs, HA scenarios) are recorded as evidence with the commit they ran at.
- `make ci` passing is not full coverage: it says nothing about rows not marked restored.

## Fast setup

`make test` keeps `-race`, `-count=1` and every assertion; the suite is kept fast by making setup cheap, not by
running tests in parallel.

- **One migration per test binary.** `internal/testsupport/dbtest.SQLite(t, dir)` runs every migration once into a
  template, then writes each test its own copy at `dir/samuori.db`; tests still call `Migrate`, which finds nothing
  to apply. It refuses to replace an existing database. Tests about migrations themselves (`internal/conversion`,
  `MigrateTo`) and `SAMUORI_TEST_POSTGRES` runs migrate normally.
- **Cheap password hashing in API tests only.** `internal/api`'s `TestMain` lowers `auth.Argon2Params`;
  `TestPasswordHashUsesProductionCost` in `internal/auth` pins the production cost, and that package's tests hash at
  full strength.
- **Parse the contract once.** The API handler parses the embedded OpenAPI contract once per process and every
  handler shares it read-only, as its requests do; `assertMatchesContract` builds its router once per test binary.
- **Discovery children skip the race runtime's exit pause.** The API tests start the sandbox child through
  `env GORACE=atexit_sleep_ms=0`; the child is still race-instrumented and exits 66 on a race, it just does not idle
  a second before exiting.
- Wait on the condition being tested (poll with a deadline), never a fixed sleep. Fixed quiet periods are only for
  negative checks, where they are the assertion (for example a removed NATS member staying unrouted).
