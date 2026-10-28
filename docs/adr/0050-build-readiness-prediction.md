# 0050. Build readiness prediction

Status: Accepted (2026-10-26) — owner request. Implementation: built 2026-10-26 with focused tests (discovery
predictions over synthetic fixtures, egress comparison under the default policy, the admission dry run and the
discovery view through the real sandbox); no real build has yet been compared with its prediction.

## Context
A real Node.js Dockerfile build (`FROM node:18-alpine`, `npm ci`, `sharp` ^0.32) failed twice. First npm was not
allowed for Dockerfile builds; after that was allowed, `sharp` 0.32's install script downloaded libvips from GitHub
releases, which the egress proxy denied. Each problem was found only after a build failed, and the denial report
(`egress.md#denied-destinations`) explained it only afterwards. The owner asked for the system to tell the developer,
for any project, what is going to break so it can be fixed before building.

## Decision
- **Static prediction, never execution.** Build readiness runs inside discovery (ADR 0024): it reads the Dockerfile,
  manifests, lockfiles, `.npmrc`, requirements files and `package.json` scripts as text. `RUN` lines and scripts are
  split into words and separators; nothing is expanded, evaluated, executed or fetched. A word holding `$` is unknown
  and predicts nothing.
- **Predictions carry a confidence.** `certain` (a literal command or URL), `likely` (a catalogued downloader or a
  registry named in a lockfile), `possible` (a manifest range without a lockfile, a start file that may be generated).
  Each item has a severity (`will-fail`, `may-fail`, `advice`), who can fix it (`developer` or `administrator`) and the
  exact fix.
- **Network needs.** Discovery maps package-manager commands to egress presets (npm, yarn, pnpm, bun → `npm`/`yarn`;
  pip, uv, poetry, pipenv → `pypi`; apk → `alpine`; apt → `debian` or `ubuntu` by base image; go → `go-proxy`; dotnet →
  `nuget`; cargo → `crates-io`; mvn/gradle → `maven-central`), registry overrides (`--registry`, `--index-url`,
  `npm_config_registry`, `GOPROXY`, `.npmrc`, requirements files, lockfile `resolved` hosts) and literal `curl`, `wget`
  and `git clone` URLs to exact `host:port` destinations. `RUN --network=none` predicts nothing. Added 2026-10-26
  (owner): Yarn Berry `.yarnrc.yml` (`npmRegistryServer`, top level and under `npmScopes`); Python indexes in
  `pip.conf` (`possible`: pip reads it only where the build installs it), Poetry, PDM and uv sources in
  `pyproject.toml` and `uv.toml`; and literal URLs inside Gradle `repositories { }` blocks of `build.gradle(.kts)` and
  `settings.gradle(.kts)` in the build context when a Dockerfile runs Gradle online.
- **A versioned catalog of install-time downloaders** (`internal/discovery/downloaders-1.0.0.json`, dated, each entry
  with its public source) maps an npm package and version range, or a command such as `playwright install`, to the
  hosts it downloads from and a fix. The lockfile's exact version is used; without a lockfile the lowest version of
  the manifest range, at `possible` confidence. Entries are skipped when the install runs with `--ignore-scripts`, when
  an environment variable the entry names (such as `PUPPETEER_SKIP_DOWNLOAD`) is set, or for development-only packages
  when the install omits them. Packages with install scripts that are not catalogued produce one `advice` item.
- **Comparison with policy happens on the controller.** Admission compares the predicted needs with the allowlist
  the build method would get (the method's presets resolved under the egress policy, plus internal destinations) and
  explains each gap with the same administrator advice as a post-build denial (`Catalog.Advise`). The analysis page
  compares Dockerfiles with Dockerfile builds and applications with everything the policy allows any build.
- **Other predictable breakages:** deprecated npm flags (`--only`, `--production`); base images past or within 90 days
  of end of life (`internal/discovery/runtime-eol-2026-10.json`, dated); a project runtime constraint (`engines.node`,
  `.nvmrc`, `requires-python`) the base image tag excludes; a literal `CMD`/`ENTRYPOINT` start file absent from the
  build context; a spec port the Dockerfile does not `EXPOSE`; rpm-based package managers no preset covers; and
  secrets the Dockerfile mounts that are undeclared or not ready.
- **Advice, not a new gate.** Readiness never changes `admissible`. Items that repeat a reason admission already
  refuses the build for are marked `blocking`. A `will-fail` item does not stop the developer from saving the spec.
- **The denial report stays the backstop.** A prediction can be wrong in both directions; the build page's denied
  destinations remain the authority on what a build actually tried to reach.

## Consequences
- The admission dry run and real admission send the build context's file list and manifest contents to the
  discovery sandbox (at most 16 MiB of content, 50 000 paths); the policy impact preview does not.
- The catalogs need upkeep: entries are dated and versioned, and wrong or missing entries are fixed by data changes,
  not code. Hosts are listed only when the package's public documentation names them.
- Predictions depend on what is literal in the files. Shell logic, variables, scripts in other languages and
  downloads by code the catalog does not know are invisible to it.

## Follow-ups
- Done 2026-10-26: `.yarnrc.yml` registries, Python index configuration (`pip.conf`, Poetry, PDM, uv) and Gradle
  repository literals (`internal/discovery/registries.go`, `TestPythonIndexConfiguration`, `TestYarnBerryRegistries`,
  `TestGradleRepositories`).
- Open: Python packages that build from source (compilers, system libraries), Gradle repositories for Railpack and
  buildpacks builds (no Java detector yet, so only Dockerfiles that run Gradle are linked), `google()` and
  `gradlePluginPortal()` shorthands, Maven `settings.xml` and `pom.xml` repositories, multi-line TOML arrays, and
  downloads by scripts in other languages.
