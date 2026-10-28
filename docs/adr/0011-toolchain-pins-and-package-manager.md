# 0011. Toolchain pins, package manager and task runner

Status: Accepted (2026-10-19). Versions verified against upstream on 2026-10-19 (`docs/dependencies.md`).

## Context
Reproducible builds need pinned toolchains, and the project must avoid unnecessary tooling.

## Decision
| Tool | Pin | Mechanism |
|---|---|---|
| Go | 1.27 language, toolchain `go1.27.2` | `go.mod`: `go 1.27.0` + `toolchain go1.27.2`; `GOTOOLCHAIN=auto` downloads it for older local Go |
| Node.js | 26.x (Active LTS from 2026-10-28; EOL 2029-04-30), pinned `26.12.0` | `.nvmrc`, `package.json` `engines`, `engine-strict=true` in `ui/.npmrc` |
| Frontend package manager | **npm** (bundled with Node; 11.20.0 with Node 26.12.0) | `package-lock.json` committed; `npm ci` in CI |
| Task runner | `make` | One root `Makefile`, compatible with macOS's GNU Make 3.81 |
| Go dev tools | `oapi-codegen`, `goose`, `govulncheck` | `tool` directives in `go.mod`, invoked with `go tool` |
| Go linter | golangci-lint v2.15.0 | Pinned release binary downloaded with checksum verification to `bin/` by a `make tools` target (not present yet; `make fmt-check` and `go vet` cover formatting and vetting). It is GPL-3.0, so it is never added to `go.mod` or linked |
| Frontend | React 19, Vite 8, TypeScript (pin at scaffold), Vitest 5, Playwright 1.64, ESLint 10, Prettier 3 | `ui/package.json` devDependencies, exact versions |

npm is chosen over pnpm/yarn because it ships with Node and needs no extra install. Since Node 25, Corepack is no
longer bundled, so pnpm/yarn would add a separate install step.

TypeScript is pinned to **5.9.3** (decided at the UI scaffold, 2026-10-19). `openapi-typescript` 7.13.0 requires
TypeScript 5.x, and `typescript-eslint` 8.71 supports up to 6.0. Revisit when both support 7.x.

## Consequences
- Contributors need only Go (any version that can bootstrap), Node 26, `make`, and for builds a Linux environment
  with BuildKit (ADR 0013).
- Renovate-style automated updates are not adopted yet; pins are bumped by PR with `docs/dependencies.md` updated.
