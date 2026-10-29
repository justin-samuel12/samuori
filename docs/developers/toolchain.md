# Toolchain and Command Reference

Status: **Authoritative.** Pins are set in ADR 0011 and verified in `docs/dependencies.md`.

> Tests: focused regression tests only (ADR 0039 as amended). `make test` runs them and `make ci` includes it.

## Prerequisites

| Need | Version | Notes |
|---|---|---|
| Go | toolchain go1.27.2 | Any Go ≥ 1.21 bootstraps it via `GOTOOLCHAIN=auto` |
| Node.js | 26.12.0 (`ui/.nvmrc`) | npm 11.20.0 comes with it. Any install method works; on a machine where another Node is global, unpack the official checksum-verified build into a private directory and set `NODE_BIN` in an untracked `local.mk` |
| make | GNU Make ≥ 3.81 | macOS system make is fine |
| Linux build environment | Ubuntu LTS with rootless BuildKit v0.34.0 | Native on Linux; Lima VM on macOS (ADR 0013) |
| Lima (macOS only) | v2.2.x | `brew install lima` |

## Make targets

| Command | Does |
|---|---|
| `make ui-install` | `npm ci` in `ui/` |
| `make generate` | Regenerates the Go server and client, the worker protocol code, the UI role table and the TS types from `api/openapi/` and `api/proto/` |
| `make check-generated` | Runs `generate` and fails if generated files differ from committed ones |
| `make fmt` / `make fmt-check` | `gofmt -s` on every Go file, writing or only checking |
| `make vet` | `go vet` on the product and `tools/apicheck` |
| `make build` | Builds `samuori`, `samuori-server`, `samuori-worker` and `samuori-web` into `bin/` |
| `make test` | Focused Go regression tests with `-race` (30-minute timeout: `internal/api` runs its tests one after another and takes about 10 minutes under the race detector), and the license-policy tests; the seccomp and sandbox tests need Linux |
| `make test-vm` | macOS: syncs the tree into the `samuori-dev` Lima VM and runs every Go test there, including the Linux-only ones |
| `make api-check` | OpenAPI validity, Samuori API rules and worker protocol rules via `tools/apicheck` |
| `make yml-check` | Fails if any tracked or untracked file ends in `.yaml` outside the exceptions below |
| `make ui-check` | Regenerates TS API types and fails on drift; then typecheck, UI unit tests, ESLint, Prettier and the Vite build |
| `make screenshots-check` | Fails when a route in `ui/src/App.tsx` has no scene in `ui/e2e/tour/scenes.ts` and no listed reason; no browser |
| `make license-check` | Fails on licenses outside the policy in `docs/dependencies.md` |
| `make sbom` | SPDX SBOM and `THIRD_PARTY_NOTICES.txt` into `dist/` |
| `make vuln` | `govulncheck` against a downloaded copy of the Go vulnerability database, offline |
| `make vuln-ui` | `npm audit --omit=dev` for `ui/` (sends the lock file to the npm registry) |
| `make ci` | `check-generated`, `fmt-check`, `vet`, `build`, `test`, `api-check`, `yml-check`, `ui-check`, `screenshots-check` and `license-check` |
| `make release` | Linux binaries, UI bundle, `LICENSE`, `NOTICE` and install files into `dist/samuori-linux-<arch>/` (`RELEASE_ARCH`, default `arm64`) |
| `make release-key`, `release-artifacts`, `release-verify`, `update` | Signed release archives and update bundles, built locally ([release.md](release.md)) |

`tools/k3s` holds the Kubernetes HA rehearsal for the `samuori-k3s` VM (`deploy/dev/lima/samuori-k3s.yml`,
`tools/k3s/provision.sh`): k3s, PostgreSQL, a three-node NATS cluster with client TLS (`deps.yml`, `secrets.sh`), the
chart with `values.yml`, and a remote worker VM. `restore.yml` rehearses a restore. Raw results are kept in the maintainer's private records.

## Running locally

| Command | Does |
|---|---|
| `make dev` | Builds the UI and `samuori-server`, then runs it on `127.0.0.1:8080` with state in `~/.cache/samuori/dev` ([local-dev.md](local-dev.md)) |
| `make dev-reset` | Deletes the `make dev` data directory |
| `make screenshots` | Captures the screenshot tour into `docs/screenshots/` on a throwaway server ([screenshots](../screenshots/README.md)) |
| `make aio-clean` | Builds a release and installs it as an all-in-one in the `samuori-clean` Lima VM |
| `make aio-compose`, `aio-debian`, `aio-amd64` | The same install via Compose, on Debian, or on amd64 under emulation |
| `make test-acceptance`, `test-acceptance-two-node` | Browser acceptance journeys against the `samuori-clean` install (and `samuori-remote` for two nodes) |

## Conventions

- **YAML extension:** every YAML file uses `.yml`. Tools that generate YAML default to `.yml`, and no file exists
  under both extensions.

  Documented exceptions, where an external tool or specification requires `.yaml`:

  | Path | Required by |
  |---|---|
  | `deploy/helm/samuori/Chart.yaml`, `deploy/helm/samuori/values.yaml` | Helm, which reads only these names (templates use `.yml`) |
  | `<chart>/Chart.yaml` and `values.yaml` in generated Kubernetes bundles (never tracked) | Helm; Samuori renames every other generated manifest to `.yml` (`architecture/kubernetes-conversion.md#validation`) |

  `go.yaml.in/yaml/v3` is a Go module path, not a file, and is not an exception.

- Go: `gofmt`; errors wrapped with `%w` and typed domain errors at API boundaries; `log/slog` structured logging;
  `context.Context` first parameter for anything doing I/O; no global mutable state.
- TypeScript: `strict` mode; no `any` in app code; API access only through `ui/src/api/client.ts`.
- SQL: lowercase keywords are fine, but one style per file; both dialects updated together.
- Code comments: see `CLAUDE.md` ("Code comments") and ADR 0020.
