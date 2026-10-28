# Dependency License and Maintenance Matrix

Status: **Living document.** Last verified **2026-10-19** from primary upstream sources, and again on **2026-10-20** for every linked Go module and UI runtime package by `make license-check` (WP40): each repository's LICENSE
file on its default branch, GitHub releases/tags, the Go module proxy, nodejs.org, postgresql.org and sqlite.org.

This is an engineering inventory of dependency licenses.

**Usage:** *Candidate* means selected in an ADR but not yet added to any manifest. When a component is added, record
the exact pinned version, re-verify its license and transitive licenses from a generated SBOM, and change its usage
to *Adopted*.

If a license cannot be verified from a primary source, mark it UNVERIFIED and treat it as unresolved. After review,
select a permissively licensed alternative instead of adopting the unverified component.

Usage kinds: **link** (compiled into Samuori binaries or UI bundle), **exec** (separate binary Samuori runs), **service**
(separate service Samuori talks to over a network API), **dev** (development/CI only, never shipped).

## Toolchains

| Component | License | Latest verified | Kind | Notes |
|---|---|---|---|---|
| Go | BSD-3-Clause | 1.27.2 (2026-10-08) | dev | Pinned toolchain (ADR 0011) |
| Node.js | MIT | 26.12.0 (Active LTS from 2026-10-28, EOL 2029-04-30); 24.22.0 enters maintenance 2026-10-20 | dev | Pinned 26.x |
| npm | Artistic-2.0 | 11.20.0 (with Node 26.12.0) | dev | Not redistributed |

## Backend (Go)

| Component | License | Latest verified | Kind | Usage | Notes |
|---|---|---|---|---|---|
| oapi-codegen | Apache-2.0 | v2.8.0 (2026-07-17) | dev | **Adopted** (`go tool`, not linked) | OpenAPI 3.1 support only initial (ADR 0010); requires Go 1.25+ |
| kin-openapi | MIT (verified, LICENSE at v0.149.0) | v0.149.0 (product); v0.151.0 (`tools/apicheck`) | link | **Adopted** in `internal/api` (request validation) and `tools/apicheck` (dev) | Pre-1.0. Raised from v0.142.0 on 2026-10-20 for GO-2026-6112 (reachable from request validation) and GO-2026-6095 (not exploitable here: Samuori authenticates before validation) |
| oapi-codegen/runtime | Apache-2.0 | v1.7.0 | link | **Adopted** (generated server and client) | |
| apapsch/go-jsonmerge | MIT | v2.0.0 (via runtime) | link | **Adopted** (transitive) | |
| pressly/goose | MIT | v3.29.0 | link/dev | **Adopted** (`internal/db`) | Migrations (ADR 0017) |
| k3s | Apache-2.0 (verified, GitHub license API) | v1.37.1+k3s1 | dev (exec, rehearsal VM) | **Adopted** for the Kubernetes HA rehearsal (`tools/k3s`) | Not bundled |
| NATS server image (`nats`) | Apache-2.0 | 2.15.1 (digest pinned) | dev (rehearsal) | Test infrastructure only | Not bundled |
| PostgreSQL image (`postgres`) | PostgreSQL License; Debian packages under their own licenses | 18 (digest pinned) | dev (rehearsal); base of the optional `controller-pgtools` image target | Test infrastructure; the image target needs a license review of its Debian base before any publication | Not published |
| Helm | Apache-2.0 (verified, GitHub license API) | v4.3.0 | dev (exec) | **Adopted** for chart checks (`tools/helm/check.sh`, dev VM) | Not bundled; the chart is optional |
| kubeconform | Apache-2.0 (verified, GitHub license API) | v0.8.0 | dev (exec) | **Adopted** for offline manifest validation | Not bundled |
| filippo.io/age | BSD-3-Clause (verified from LICENSE) | v1.3.2 | link | **Adopted** (`internal/backup`) | Backup encryption (ADR 0035); transitive `filippo.io/hpke` v0.4.0, BSD-3-Clause (verified from LICENSE) |
| PostgreSQL `pg_dump`, `pg_restore` | PostgreSQL License | server version or newer | exec (operator-installed) | **Adopted** for PostgreSQL backups | Not bundled |
| jackc/pgx | MIT | v5.11.0 | link | **Adopted** (`internal/db`) | |
| modernc.org/sqlite | BSD-3-Clause (SQLite C is public domain) | v1.60.1 | link | **Adopted** (`internal/db`) | Single maintainer |
| nats-server | Apache-2.0 | v2.15.1 | link | **Adopted** (`internal/messaging`) | Embedded with no network listener; transitives nats-io/jwt, nkeys, nuid, minio/highwayhash, google/go-tpm (Apache-2.0), klauspost/compress (BSD-3-Clause), antithesis-sdk-go no-op (MIT), licenses verified |
| nats.go | Apache-2.0 | v1.54.0 | link | **Adopted** (`internal/messaging`) | In-process connection only |
| santhosh-tekuri/jsonschema | Apache-2.0 | v6.0.3 | link | **Adopted** in the root module (`internal/buildspec`) | JSON Schema 2020-12; single maintainer |
| go.yaml.in/yaml/v3 | Apache-2.0 (+ MIT portions), NOTICE | v3.0.5 | link | **Adopted** in the root module (`internal/buildspec`) | `gopkg.in/yaml.v3` is **archived**, do not use; v4 still RC |
| coreos/go-oidc | Apache-2.0, NOTICE (verified from LICENSE, 2026-10-20) | v3.21.0 | link | **Adopted** (`internal/adapters/identity/oidc`) | Transitives golang.org/x/oauth2 v0.37.0 (BSD-3-Clause) and go-jose/go-jose/v4 v4.1.5 (Apache-2.0), each verified from its LICENSE |
| go-jose/go-jose (v4, `jose` and `jwt`) | Apache-2.0 (verified, LICENSE at v4.1.5) | v4.1.5 | link | **Adopted** for mirror-token JWT signing (`internal/imagesources`), replacing hand-written JWS code; also a go-oidc transitive | |
| go-ldap/ldap | MIT (verified from LICENSE, 2026-10-20) | v3.4.15 | link | **Adopted** (`internal/adapters/identity/ldap`) | Transitives go-asn1-ber/asn1-ber v1.5.8 (MIT), Azure/go-ntlmssp v0.1.1 (MIT), google/uuid (BSD-3-Clause), each verified from its LICENSE |
| OpenLDAP slapd | OpenLDAP Public License 2.8 (BSD-style) | 2.6.10 (Ubuntu package) | dev | **Dev-only service** in the dev VM (`provision-ldap.sh`) | Started per test as a throwaway server; never bundled |
| go-git (`github.com/go-git/go-git/v5`: plumbing transport, packfile, object and filesystem storage only) and go-billy | Apache-2.0 (verified from each LICENSE, 2026-10-20) | v5.19.3 (2026-10-04); go-billy v5.9.2 | link | **Adopted** (`internal/adapters/git/gogit`, controller only; ADR 0033) | Transitives verified from each LICENSE: ProtonMail/go-crypto v1.4.1 (BSD-3-Clause), cloudflare/circl v1.6.3 (BSD-3-Clause), cyphar/filepath-securejoin v0.7.0 (BSD-3-Clause for every linked file; the module also holds MPL-2.0 files under `pathrs-lite`, which are not linked), emirpasic/gods v1.18.1 (BSD-2-Clause), go-git/gcfg (BSD-3-Clause), golang/groupcache (Apache-2.0), jbenet/go-context (MIT), pjbgf/sha1cd v0.7.0 (Apache-2.0), sergi/go-diff (MIT), gopkg.in/warnings.v0 v0.1.2 (BSD-2-Clause). The SSH transport is not imported |
| Git (`git http-backend`) | GPL-2.0 | system package | dev | **Removed** with the test support packages (2026-10-20, ADR 0039) | Samuori never runs git |
| google/go-containerregistry | Apache-2.0 (verified from LICENSE) | v0.21.7 (root module, tests) and v0.22.1 (via BuildKit) | link (transitive via BuildKit); test | **Adopted**: `pkg/registry` as an in-process test registry only; also a BuildKit transitive dependency | Pre-1.0 |
| oras-go (`oras.land/oras-go/v2`) | Apache-2.0 (verified from LICENSE) | v2.6.4 | link | **Adopted** (`internal/publication`, controller only) | Referrer-aware copy (spike oci-copy); transitive `golang.org/x/sync` (BSD-3-Clause) |
| OpenTelemetry Go (`otel`, `otel/trace`, `otel/sdk`, `otel/exporters/otlp/otlptrace/otlptracehttp`) | Apache-2.0 (verified from each module's LICENSE, 2026-10-20) | v1.47.0 | link | **Adopted** (`internal/telemetry` only; domain packages use its `Span` wrapper) | OTLP/HTTP trace export, off unless an endpoint is configured; transitives `go.opentelemetry.io/proto/otlp` (Apache-2.0, already a BuildKit transitive) and cenkalti/backoff/v5 v5.0.3 (MIT) |
| prometheus/client_golang | Apache-2.0, NOTICE (verified from LICENSE, 2026-10-20) | v1.25.0 | link | **Adopted** (`internal/telemetry` only) | Metrics registry and text exposition; transitives prometheus/common v0.72.0, procfs v0.22.0 and client_model v0.6.3 (Apache-2.0, NOTICE), beorn7/perks v1.0.1 (MIT), munnerz/goautoneg (BSD-3-Clause), each verified from its LICENSE |
| aws-sdk-go-v2 (core, `config`, `credentials`, `service/secretsmanager`, `service/kms`, `service/ecr`, `service/s3`, `service/sts`) and smithy-go | Apache-2.0, NOTICE (verified from LICENSE, 2026-10-20) | core v1.47.3; config v1.33.9; credentials v1.20.9; smithy-go v1.28.5; secretsmanager v1.50.5; kms v1.61.5; ecr v1.66.5; s3 v1.114.3; sts v1.51.4 | link | **Adopted** (`internal/adapters/secretstore/awssm`, `internal/adapters/kms`, `internal/adapters/registry/ecr`, `internal/adapters/storage/s3`) | Already a BuildKit transitive |
| OpenBao | MPL-2.0 | 2.7.1 | dev | **Dev-only service** in the dev VM (`provision-openbao.sh`; checksum from the Sigstore-verified release `checksums.txt`) | Reached over HTTP; operators run their own in production |
| Prometheus | Apache-2.0 | 3.15.0 (2026-09-25; checksum from the release `sha256sums.txt`) | dev | **Dev-only service** in the dev VM (`provision-observability.sh`) | Interop test of the metrics listener; operators run their own |
| zot (minimal build) | Apache-2.0 | 2.1.22 (2026-10-06; checksum from the release `checksums.sha256.txt`) | dev | **Dev-only service** in the dev VM (`provision-zot.sh`) | A registry with the OCI 1.1 referrers API, for the referrers publication test |
| OpenTelemetry Collector (core `otelcol`) | Apache-2.0 | 0.162.0 (2026-09-29; checksum and Sigstore bundle verified) | dev | **Dev-only service** in the dev VM (`provision-observability.sh`) | Interop test of OTLP/HTTP tracing; operators run their own |
| moto (`moto[server]`) | Apache-2.0 | 5.2.3 | dev | **Dev-only AWS API emulator** in the dev VM (`provision-aws-emulator.sh`) | Does not implement KMS `MessageType=DIGEST` (see `signing.md`) |
| BuildKit Dockerfile parser (`github.com/moby/buildkit/frontend/dockerfile/{parser,instructions}`) | Apache-2.0 | v0.34.0 | link | **Adopted** (`internal/discovery`) | Static Dockerfile analysis (ADR 0028); runs in the discovery sandbox |
| BuildKit parser transitives: agext/levenshtein v1.2.3, docker/go-units v0.5.0, moby/docker-image-spec v1.3.1, opencontainers/go-digest v1.0.0, opencontainers/image-spec v1.1.1, containerd/typeurl/v2 v2.3.0 (Apache-2.0); pkg/errors v0.9.1 (BSD-2-Clause); planetscale/vtprotobuf (BSD-3-Clause); tonistiigi/go-csvvalue (MIT); google.golang.org/protobuf v1.36.12 (BSD-3-Clause) | as listed | as listed | link | **Adopted** (transitive) | Licenses verified from each module's LICENSE file |
| BuildKit client (`github.com/moby/buildkit/client`, `session`, `session/secrets/secretsprovider`) and `tonistiigi/fsutil` | Apache-2.0; fsutil MIT | v0.34.0; fsutil v0.0.0-20260819142231-83cac42c1c52 | link | **Adopted** (`internal/adapters/build/buildkit`, worker only) | Session secret delivery needs the client library (OD-15); replaces `buildctl` |
| BuildKit client transitives (2026-10-19, from each module's LICENSE): containerd/containerd/v2 v2.4.1 (Apache-2.0, NOTICE), containerd/{api v1.12.0, continuity v0.5.0, errdefs v1.0.0, errdefs/pkg v0.3.0, log v0.2.0, log/otel v0.1.0, platforms v1.0.0-rc.5, ttrpc v1.2.10}, distribution/reference v0.6.0, go-logr/{logr v1.4.4, stdr v1.2.2}, google/shlex, in-toto/{attestation v1.2.0, in-toto-golang v0.11.0}, moby/{locker v1.0.1, sys/signal v0.7.1}, moby/patternmatcher v0.6.1 (NOTICE), shibumi/go-pathspec v1.3.0, go.opentelemetry.io/{otel, otel/trace, otel/metric, otel/sdk, otel/log v1.47.0, otel/exporters/otlp/otlptrace v1.46.0, proto/otlp v1.11.0, auto/sdk v1.2.1, contrib otelgrpc/otelhttptrace v0.71.0, otelhttp v0.72.0}, google.golang.org/genproto/googleapis/api (Apache-2.0); Microsoft/go-winio v0.6.3, cespare/xxhash/v2 v2.3.0, felixge/httpsnoop v1.1.0, secure-systems-lab/go-securesystemslib v0.11.0, sirupsen/logrus v1.10.2, tonistiigi/units (MIT); gofrs/flock v0.13.1, grpc-ecosystem/grpc-gateway/v2 v2.30.0 (BSD-3-Clause) | as listed | as listed | link | **Adopted** (transitive) | All permissive; NOTICE files to carry in distributions |
| pelletier/go-toml/v2 | MIT | v2.4.3 | link | **Adopted** (`internal/discovery`) | Reads `pyproject.toml`; license verified from the module LICENSE |
| golang.org/x/sys | BSD-3-Clause | v0.49.0 | link | **Adopted** (`internal/discovery/sandbox`) | seccomp and rlimits for the discovery sandbox |
| golang.org/x/crypto | BSD-3-Clause | v0.58.0 | link | **Adopted** in the root module (`internal/auth`) | argon2id password hashing; license verified from the module LICENSE |
| golang.org/x/text | BSD-3-Clause | v0.43.0 | link | **Adopted** in the root module (`internal/buildspec`, `internal/sources`) | Error message printer and case folding |
| bufbuild/protocompile | Apache-2.0 | v0.14.1 | dev | **Adopted** in `tools/apicheck` | Compiles and checks `api/proto/`; license to re-verify when pinned for product codegen |
| google.golang.org/protobuf | BSD-3-Clause | v1.36.12 (root module and `tools/protogen`); v1.34.2 in `tools/apicheck` | link | **Adopted** | Worker protocol messages |
| google.golang.org/grpc | Apache-2.0 | v1.84.0 | link | **Adopted** (`internal/workers`, `internal/workeragent`) | Worker protocol (OD-20); pulls golang.org/x/net (BSD-3-Clause) and google.golang.org/genproto/googleapis/rpc (Apache-2.0) |
| protoc-gen-go / protoc-gen-go-grpc | BSD-3-Clause / Apache-2.0 | v1.36.12 / v1.6.2 | dev | **Adopted** as Go tools in `tools/protogen` | Run by `make generate` over descriptors from protocompile; no system protoc |
| golangci-lint | **GPL-3.0** | v2.15.0 | dev | Candidate | CI binary only; never in `go.mod`, never linked |
| govulncheck (x/vuln) | BSD-3-Clause (verified, LICENSE at v1.8.0) | v1.8.0 (v1.9.0 of 2026-10-11 not yet adopted) | dev | **Adopted**: `make vuln`, run offline against the downloaded Go vulnerability database | `go run` at a pinned version; not in `go.mod` |
| google/licensecheck | BSD-3-Clause (verified, LICENSE at v0.3.1) | v0.3.1 | dev | **Adopted** in `tools/licensecheck` (`make license-check`, `make sbom`) | License text classifier; own module, never linked into Samuori |

## Frontend

| Component | License | Latest verified | Kind | Usage | Notes |
|---|---|---|---|---|---|
| React | MIT | 19.3.1 | link | **Adopted** (`ui/`, exact pin) | Repo moved to `react/react` |
| TypeScript | Apache-2.0, NOTICE | 5.9.3 pinned (7.0.2 is latest) | dev | **Adopted** (`ui/`) | 5.x required by openapi-typescript 7.13.0 (ADR 0011) |
| Vite | MIT | 8.3.4 | dev | **Adopted** (`ui/`, exact pin) | |
| Vitest | MIT | 5.0.5 | dev | **Removed** (2026-10-20, ADR 0039) | |
| Playwright (`@playwright/test`) | Apache-2.0, NOTICE | 1.64.1 | dev | **Adopted** (`ui/`, exact pin) for the browser acceptance journey (`make test-acceptance`, ADR 0039 amendment) | Browser E2E; downloaded browsers carry their own licenses and are never redistributed |
| axe-core / `@axe-core/playwright` | MPL-2.0 | 4.13.0 | dev | **Adopted** (`ui/`, exact pin) for the browser acceptance journey (`make test-acceptance`, ADR 0039 amendment) | Accessibility rules in E2E. Weak copyleft applies to modified axe files; Samuori neither modifies nor ships it |
| `@types/node` | MIT | 26.6.3 | dev | **Adopted** (`ui/`, exact pin) | Types for E2E and Playwright config only |
| ESLint | MIT | 10.13.0 | dev | **Adopted** (`ui/`, exact pin) | |
| Prettier | MIT | 3.9.10 | dev | **Adopted** (`ui/`, exact pin) | |
| openapi-typescript | MIT | 7.13.0 (2026-02-11) | dev | **Adopted** (`ui/`, exact pin) | Slow release cadence |
| openapi-fetch | MIT | 0.17.0 | link | **Adopted** (`ui/`, exact pin) | Pre-1.0 |
| React Aria Components | Apache-2.0, NOTICE | 1.22.1 | link | **Adopted** (`ui/`, exact pin) | Accessibility-first, unstyled |
| Tailwind CSS (`tailwindcss`, `@tailwindcss/vite`) | MIT | 4.3.3 | dev | **Adopted** (`ui/`, exact pin, 2026-10-21, ADR 0023 amendment) | Build-time CSS generation; theme and utilities only, no preflight reset |
| TanStack Query (`@tanstack/react-query`, `query-core`) | MIT | 5.104.1 | link | **Adopted** (`ui/`, exact pin, 2026-10-21) | Server-state fetching and caching for redesigned pages |
| TanStack Table (`@tanstack/react-table`, `table-core`) | MIT | 8.21.3 (9.x is a new API, not adopted) | link | **Adopted** (`ui/`, exact pin, 2026-10-21) | Headless sorting; React Aria still renders the accessible grid |
| Lucide (`lucide-react`) | ISC; some icons MIT (from Feather) | 1.52.0 | link | **Adopted** (`ui/`, exact pin, 2026-10-21) | Icons; license text carried in notices |
| DejaVu Sans Bold (outlined glyphs) | Bitstream Vera license; DejaVu changes public domain | not recorded by the logo source | asset | **Adopted** (`docs/brand/`, `ui/`, 2026-10-27) | Wordmark letters only, as SVG paths; no font file shipped; notice kept with the brand files and UI (ADR 0057) |
| Radix UI Primitives | MIT | radix-ui 1.8.1 | link | Alternative | |
| MUI core | MIT | 9.5.0 | link | Alternative | MUI X Pro/Premium is commercial, avoid |

## Build, execution and registry

| Component | License | Latest verified | Kind | Usage | Notes |
|---|---|---|---|---|---|
| BuildKit (`buildkitd`, `buildctl`) | Apache-2.0 | v0.34.0 (2026-10-07) | exec | **Adopted** (`internal/adapters/build/buildkit`) | Not bundled; installed on worker hosts (dev VM: `deploy/dev/lima/provision-buildkit.sh`, checksum-pinned) |
| RootlessKit | Apache-2.0 (verified from LICENSE at v3.3.0) | v3.3.0 | exec | **Adopted** (`internal/adapters/build/buildkit`) | Not bundled; checksum-pinned in the dev VM provisioner |
| slirp4netns | GPL-2.0-or-later (Debian copyright file; some files BSD-3-Clause) | 1.3.3 (Ubuntu package) | exec-only | **Adopted** (dev VM and worker hosts) | Rootless networking for buildkitd; OS package, never bundled or linked |
| uidmap (shadow `newuidmap`/`newgidmap`) | BSD-3-Clause (Debian copyright file) | 4.17.4 (Ubuntu package) | exec-only | **Adopted** (dev VM and worker hosts) | Subordinate ID mapping for rootless BuildKit; OS package, never bundled |
| nftables (`nft`) | GPL-2.0 (Debian copyright file) | 1.1.6 (Ubuntu package) | exec-only, by operators | **Adopted** (worker hosts) | Applies the ruleset from `samuori-worker egress-rules`; never bundled or linked |
| runc | Apache-2.0, NOTICE | v1.5.2 | exec | Candidate | |
| crun | **GPL-2.0-or-later** / LGPL-2.1 (libcrun) | 1.30.1 | exec | Optional | Execute as separate binary only; never link |
| Buildah | Apache-2.0 | v1.45.2 upstream; 1.42.1 tested (Ubuntu 26.04 package) | exec | **Adopted (exec)** as the optional second engine (`internal/adapters/build/buildah`); dev VM provisioning `deploy/dev/lima/provision-buildah.sh` | Repo moved to `podman-container-tools` (CNCF Sandbox). Runtime dependencies used in the spike: runc 1.4.0 (Apache-2.0), netavark 1.16.1 (Apache-2.0), containers-common (Apache-2.0), slirp4netns (GPL-2.0, exec only, already used through RootlessKit). crun (GPL-2.0) deliberately not installed |
| Podman | Apache-2.0 | v6.1.4 | exec | Future adapter | |
| Skopeo | Apache-2.0 | v1.24.1 | exec | Candidate | Image copy |
| Shipwright Build | Apache-2.0 | v0.21.9 | service | Optional adapter | Requires Kubernetes; pre-1.0 |
| Railpack (CLI, `railpack-frontend`, `railpack-builder` and `railpack-runtime` images) | MIT (verified from the release archive's LICENSE, 2026-10-24) | v0.43.0 (2026-10-19); images `mise-2026.10.14` | exec / BuildKit frontend / base images | **Adopted (ADR 0047)**, optional, disabled until an administrator approves a version | Not bundled: `install.sh --railpack` installs the upstream linux-musl binary checked against upstream `checksums.txt` sums pinned in the script. Planning runs `mise` (MIT, verified 2026-10-24 from jdx/mise LICENSE) 2026.10.14, which `install.sh --railpack` installs from the upstream linux-musl archive checked against mise's `SHASUMS256.txt` and the worker verifies before staging, instead of Railpack's own unchecked download; runs only inside build isolation; frontend and images pinned by digest |
| buildpacks/pack, lifecycle | Apache-2.0 | lifecycle v0.21.26 (in the builder); `pack` v0.40.9 not used | exec inside a BuildKit build | **Adopted lifecycle (ADR 0047)**, optional, disabled until an administrator approves a builder; `pack` not used | Runs only inside build isolation from the approved builder image pinned by digest, which carries it (`/cnb/lifecycle`); Samuori drives platform API 0.14 with the experimental OCI-layout export (`CNB_EXPERIMENTAL_MODE=warn`). Detect and build run buildpack code and the app's dependency installs |
| Paketo builder and run images, buildpacks | Apache-2.0 | verified pins (2026-10-24): `paketobuildpacks/ubuntu-noble-builder:0.0.205@sha256:39dd8643…07c4f`, `ubuntu-noble-run:0.0.150@sha256:2b7308ae…ab060` (amd64 and arm64) | builder and base image (exec in isolation) | **Adopted (ADR 0047)**, optional, administrator-approved digests | Images contain Ubuntu packages and buildpack dependencies (Node.js, npm and others) under their own licenses; never bundled with Samuori. `paketobuildpacks/builder-jammy-base:latest` is amd64-only |
| Kompose | Apache-2.0 (verified, GitHub license API, 2026-10-24) | v1.38.0 (2026-01-15) | exec (worker, inside a conversion step) | **Adopted (ADR 0048)**, optional, disabled until an administrator approves a version and the per-architecture SHA-256 | Static Go binary; never bundled. `install.sh --with-kompose` installs the release's `kompose-linux-<arch>` checked against its `SHA256_SUM` (amd64 `65a6a720…c147`, arm64 `24faf621…9909`); runs with `--build none`, no network and no credentials (`architecture/kubernetes-conversion.md`) |
| Kubernetes OpenAPI document (`api/openapi-spec/swagger.json`, kubernetes/kubernetes) | Apache-2.0 (verified, GitHub license API, 2026-10-24) | v1.36.5 | data (embedded) | **Adopted (ADR 0048)**: `tools/kubeschema` extracts the schemas of 18 kinds into `internal/kubeconvert/schemas/kubernetes-v1.36.json` (99 KB) | Derived data; validated with santhosh-tekuri/jsonschema (already adopted); no new Go module |
| Move2Kube | Apache-2.0 (verified, release archive `LICENSE` and GitHub, 2026-10-24) | v0.3.15 (2025-03-07) | exec (worker, inside a conversion step) | **Adopted (ADR 0048)**, optional, **disabled by default** | Unmaintained: last release 2025-03-07, built from the last commit on `main` (2024-11-06), checked 2026-10-24. Static Go binary; never bundled. `install.sh --with-move2kube` installs `move2kube-v0.3.15-linux-<arch>.tar.gz` checked against its `.sha256sum` (amd64 `5bcda9f3…dc5c0f`, arm64 `d2a28dea…5977bd`); binary SHA-256 to approve: amd64 `7f63f3ab…afb501`, arm64 `2edf77ac…031855`. Runs `transform --qa-skip --disable-local-execution --ignore-env` with no network, no credentials and no container runtime (`architecture/kubernetes-conversion.md#move2kube`, `spikes/move2kube.md`) |
| Kata Containers | Apache-2.0 | 4.2.0 | service | Candidate (isolation) | Guest kernel/hypervisor have own licenses |
| Cloud Hypervisor | Apache-2.0 AND BSD-3-Clause (verified: `cloud-hypervisor/Cargo.toml` and `LICENSES/` at v53.0; the static binary also contains Rust crates under their own licenses, not yet inventoried) | v53.0 (2026-07-12; v54.0 of 2026-10-14 not yet adopted) | exec | **Adopted (exec)** by the `kvm-cloud-hypervisor` provider (ADR 0038) | Never bundled; installed on worker hosts with the release asset digests (`deploy/dev/lima/provision-kvm.sh`). Not yet validated: see ADR 0038 |
| mmdebstrap | Expat/MIT (verified, Debian copyright file) | 1.5.7-3 (Ubuntu 26.04 package) | dev/operator tool | **Adopted** by `tools/vm-image/build.sh` | Runs on the host that builds the guest image; never bundled |
| Guest image contents (Ubuntu 26.04 packages, Linux kernel) | Package licenses, including GPL-2.0 (kernel, util-linux and others); not yet inventoried | built from the Ubuntu archive at image-build time | operator-built VM image | Built by the operator with `tools/vm-image`; Samuori distributes no guest image | An inventory and license review are needed before any guest image is published |
| Lima | Apache-2.0, NOTICE | v2.2.1 | dev | Candidate | macOS dev VM |
| Colima | MIT | v0.10.3 | dev | Not adopted | |
| Distribution registry | Apache-2.0 | v3.1.2 | service | **Adopted**: dev VM mirror (`provision-egress.sh`), all-in-one mirror and local registry (`deploy/systemd/install.sh`, checksum-pinned), Compose image `registry:3.1.2` (digest-pinned) | Not bundled; downloaded at install |
| Harbor | Apache-2.0 | v2.15.4 | service | Optional | |
| Distroless static (`gcr.io/distroless/static-debian12:nonroot`) | Apache-2.0 (project); the image holds Debian packages under their own licenses, not yet inventoried | digest `sha256:afa5c872…` | base image | **Adopted** for the Compose controller image (`deploy/compose/Dockerfile`) | Not distributed by Samuori; built locally by the operator |
| Alpine (`alpine:3.22`) | Package licenses, including BusyBox (GPL-2.0); not yet inventoried | digest `sha256:5291449c…` | build stage | **Adopted** as a download stage only (`deploy/compose/Dockerfile`) | Nothing from it reaches the final image except the checksum-verified Syft binary |
| Docker Engine and Compose v2 | Apache-2.0 | 29.1.3, 2.40.3 (Ubuntu packages) | operator tool | Used by operators and in the Compose acceptance VM | Not bundled or required by the native install |

## Security

| Component | License | Latest verified | Kind | Usage | Notes |
|---|---|---|---|---|---|
| Trivy | Apache-2.0, NOTICE | v0.76.0 | exec | Scanner option, operator-installed and selected (`validation.vulnerabilityScanner: trivy`); adapter in `internal/validation/vuln.go`; no default until DB terms verified (ADR 0021, OD-10) | Not bundled or downloaded by Samuori. Vulnerability DB data terms **UNVERIFIED** |
| ssh-keygen (openssh-client) | BSD-style (OpenSSH LICENCE) | Ubuntu or Debian package | exec | **Adopted** (`internal/selfupdate`, `samuori-self-update`) | Verifies release signatures; installed by the installers from the distribution archive, not bundled |
| Syft | Apache-2.0 (verified from LICENSE at v1.54.1) | v1.54.1 (2026-10-06) | exec | **Adopted** (`internal/validation`, optional on the controller) | Not bundled; run without network; dev VM install checksum-pinned (`provision-validation.sh`). Also for Samuori's own SBOM |
| Grype | Apache-2.0 | v0.120.1 | exec | Scanner option, operator-installed and selected (`validation.vulnerabilityScanner: grype`); adapter in `internal/validation/vuln.go`; no default until DB terms verified (ADR 0021, OD-10) | Not bundled or downloaded by Samuori. Dev VM install checksum-pinned (`provision-scanner.sh`) for the live adapter test. DB data terms **UNVERIFIED**; the dev VM downloads the database for development use only |
| Cosign | Apache-2.0 (verified from LICENSE at v3.1.3) | v3.1.3 | exec | **Test-only**: interoperability check in the dev VM (`provision-signing.sh`, checksum-pinned). Samuori writes Cosign-format signatures itself (`internal/signing`) and neither links nor bundles Cosign | v3 changed flags from v2 |
| OPA | Apache-2.0 | v1.21.2 | link/service | Optional | |
| Secret/key content scanner for sensitive outputs | — | — | exec | Open (ADR 0031) | Candidates: Trivy secret scanning (Apache-2.0) or gitleaks (MIT); to be verified and pinned |

## Identity, secrets, observability, backup, databases

| Component | License | Latest verified | Kind | Usage | Notes |
|---|---|---|---|---|---|
| OpenBao | **MPL-2.0** | v2.7.1 | service | Preferred external secrets/PKI | Do not vendor or modify files (ADR 0015) |
| HashiCorp Vault | **BUSL-1.1** (since 2023-08-11, v1.15.0+) | v2.2.0 | service | Compatibility adapter only | Not OSI; never bundled; operator-provided |
| Keycloak | Apache-2.0 | 26.8.1 | service | Optional OIDC provider | |
| Grafana | **AGPL-3.0-only** | v13.2.4 | service | Optional external | Never bundled (ADR 0016) |
| Loki | **AGPL-3.0-only** (some subpackages Apache-2.0) | v3.7.8 | service | Optional external | Never bundled |
| Prometheus | Apache-2.0, NOTICE | v3.15.0 | service | Optional external | |
| Kopia | Apache-2.0 | v0.23.1 | exec | Optional backup adapter | Pre-1.0 |
| pgBackRest | MIT | 2.60.0 | exec | Optional backup adapter | |
| PostgreSQL | PostgreSQL License | 18.6; supported 16–18 by Samuori | service | Candidate | 14 EOL 2026-11-12 |
| SQLite | Public domain (`blessing`) | 3.54.1 | link (via modernc) | Candidate | |

## License policy (enforced by `make license-check`)

Engineering policy. `tools/licensecheck` lists every Go module linked into `samuori-server`,
`samuori-worker` and `samuori` for linux/amd64 and linux/arm64 (`go list -deps`), and every UI runtime package in
`ui/package-lock.json`. It classifies each one from its license files (google/licensecheck; declared license for
npm packages) and fails on any license outside the allowed set unless an engineering review in
`tools/licensecheck/reviewed.json` covers exactly that license and, where files differ, those packages. A review
records facts about what is linked. `make sbom` writes an SPDX 2.3 SBOM and the
third-party license and notice texts to `dist/`.

| Category | Licenses | link | exec / service | dev |
|---|---|---|---|---|
| Allowed | Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause, ISC, 0BSD, Zlib, PostgreSQL, blessing / public domain | yes | yes | yes |
| Review required (record in this file) | MPL-2.0, LGPL-2.1/3.0, Artistic-2.0, EPL-2.0, CC-BY-4.0 (data), unknown or UNVERIFIED | no, unless approved | case by case | yes |
| Denied in shipped artifacts | GPL-2.0/3.0, AGPL-3.0, SSPL, BUSL-1.1, Elastic-2.0, any non-commercial or field-of-use license | no | external, operator-provided only (never bundled) | GPL allowed for CI tools that are not distributed |

## Security and maintenance review

The 2026-10-19 pass recorded license, latest release and maintenance signals only. A per-component security review
happens when a component is adopted and is recorded here:

- open advisories (OSV / GitHub advisories; `govulncheck` and `npm audit` for linked code)
- upstream security policy and disclosure process
- release signing or provenance availability
- the Samuori threat-model notes for the adapter (`docs/architecture/integrations.md` gate 4)

Status (2026-10-20): `make vuln` checks every linked Go module against the Go vulnerability database (offline), and
the reachable findings were fixed (kin-openapi above). The remaining module-level report is GO-2026-5932
(`golang.org/x/crypto/openpgp`, which Samuori does not import). UI runtime packages are checked with `make vuln-ui`
(`npm audit`), which sends the lockfile to the npm registry and has not been run. Security policies, release
signing and provenance for each adopted component are not yet reviewed.

## UI dependency tree (2026-10-19, from `ui/package-lock.json`)

Runtime dependencies are all MIT, ISC, Apache-2.0 or BSD. Dev-only packages that need review under the policy:
- `lightningcss` (MPL-2.0), Vite's CSS processor, used unmodified as a build tool;
- `@csstools/*` (MIT-0);
- `type-fest` (MIT OR CC0-1.0).

No GPL, AGPL or source-available licenses. Re-run this check whenever `package-lock.json` changes; it moves into
`make license-check` later.

## Flags requiring attention

- **Not OSI / changed license:** Vault (BUSL-1.1). Never a dependency.
- **Network copyleft:** Grafana, Loki (AGPL). External only.
- **Strong copyleft tools:** golangci-lint (GPL-3.0, CI only); crun (GPL/LGPL, exec only).
- **File-level copyleft:** OpenBao (MPL-2.0), used as an unmodified service.
- **Archived:** `gopkg.in/yaml.v3`.
- **Executes code during "planning":** Railpack (`mise`), CNB detect.
- **UNVERIFIED:** Trivy/Grype vulnerability DB data terms; Kata
  governance.
- **NOTICE and license texts:** `make sbom` collects the license and NOTICE files of every linked module and UI
  runtime package into `dist/THIRD_PARTY_NOTICES.txt` for distributions. Samuori's own `LICENSE` (Apache-2.0) and `NOTICE`
  ship in every release archive ([ADR 0056](adr/0056-apache-2-license.md)).
- **Per-file licensing:** `cyphar/filepath-securejoin` mixes BSD-3-Clause and MPL-2.0 files; only BSD-3-Clause
  packages are linked, and `make license-check` fails if that changes.
