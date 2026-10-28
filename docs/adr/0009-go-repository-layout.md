# 0009. Idiomatic Go repository layout

Status: Accepted (2026-10-19). Names as in ADR 0057.

## Context
The original layout placed unit tests in `tests/unit/` and adapters in a top-level `adapters/` directory, which is
importable by other modules and conflicts with Go convention of colocated `_test.go` files.

## Decision
```
README.md  LICENSE  NOTICE  CONTRIBUTING.md  SECURITY.md  CODE_OF_CONDUCT.md  CLAUDE.md  Makefile  go.mod
.github/workflows/                                  CI
cmd/samuori-server/  cmd/samuori-worker/  cmd/samuori/          binaries (thin main packages)
internal/<domain>/                                  domain packages (api, auth, policy, projects, discovery, spec,
                                                    builds, scheduler, workers, messaging, storage, registries,
                                                    secrets, security, telemetry, notifications, backup, upgrades)
internal/adapters/<kind>/<impl>/                    detection, build, registry, storage, secrets, identity, ...
internal/apiclient/                                 generated Go client (used by samuori CLI and tests)
api/openapi/  api/schemas/                          contracts (source of truth for generation)
migrations/sqlite/  migrations/postgres/
ui/                                                 React app (own package.json)
test/integration/  test/e2e/  test/security/  test/chaos/  test/performance/
deploy/compose/  deploy/systemd/  deploy/helm/  deploy/dev/
examples/node-api/  examples/python-api/  examples/powershell/
tools/                                              small repo scripts only
docs/
```

- Unit tests are colocated `_test.go` files in the package under test. Fixtures live in package `testdata/` dirs.
- `test/` holds only cross-package suites, gated by build tags (`integration`, `e2e`, `security`, `chaos`,
  `performance`) so `go test ./...` stays fast and hermetic.
- Everything is under `internal/` until a public Go SDK is a deliberate decision. No `pkg/` directory. The
  exceptions are `api/schemas` and `migrations`, tiny packages that embed their files so there is a single copy.
- Domain packages define the narrow interfaces they need; adapters implement them. Domain packages never import
  vendor SDKs.
- One Go module at the repository root for the product. Tools and spikes under `tools/` are separate modules, so
  their dependencies never enter product builds.

## Consequences
Go tooling (`go test ./...`, `go vet`, coverage) works with no custom configuration.
