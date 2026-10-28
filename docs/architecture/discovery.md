# Discovery

Status: **Authoritative** for contracts and safety rules. Confidence weights are a starting point to be tuned with
fixtures; changes require updating this document and the unit tests together.

## Safety

- Discovery never executes uploaded code, dependencies, package scripts or lifecycle hooks.
- Detectors read files from an immutable snapshot through a read-only, size-bounded file API; they have no network
  access and no ability to spawn processes. They run in a short-lived, resource-limited subprocess, never inside
  the API process (ADR 0024).
- Third-party plan providers are disabled by default and are never part of the safe baseline:
  - **Railpack** `plan`/`info` do not run the app's code or package scripts, but they download the `mise` binary at
    runtime and execute it over uploader-controlled files (2026-10 source review). Allowed only in an
    egress-denied, read-only sandbox with a pre-staged, pinned `mise`, after the M0 spike confirms behavior.
  - **Cloud Native Buildpacks** detect runs buildpack binaries against the source, so it is a build-phase activity,
    not discovery. It may run only inside the isolated build environment.
- Malformed and giant manifests are handled with parse limits (bytes, depth, entry counts) and produce warnings,
  not crashes.

## Sandbox implementation

`samuori-server` re-runs itself as `samuori-server discover` for each discovery (`internal/discovery/sandbox`). The
controller sends the snapshot's path list and the content of the files detectors read (manifests, project files,
scripts, Dockerfiles, `.npmrc`, `.yarnrc.yml`, `pip.conf`, `uv.toml`, Gradle build and settings scripts, and the Python entry modules `main.py`, `app.py`, `asgi.py`, `wsgi.py` and `server.py` up to 256 KiB; at most 1 MiB each, npm, yarn and pnpm lockfiles up to 8 MiB, and 32 MiB in total) on stdin, with an empty environment, and reads at most
8 MiB of JSON back. Before reading input, the child:

- sets `RLIMIT_CPU` (30 s), `RLIMIT_AS` (4 GiB), `RLIMIT_FSIZE` 0, `RLIMIT_NOFILE` 64 and no core dumps;
- sets `no_new_privs` and installs a seccomp filter (all threads). It kills the process on any architecture other
  than the native one (so the 32-bit compat ABIs are refused) and, on amd64, on every x32-ABI call, which would
  otherwise share the native architecture and bypass the number checks. It fails with `EPERM`:
  - program execution (`execve`, `execveat`) and socket creation (`socket`, `socketpair`, `connect`);
  - io_uring (`io_uring_setup`, `_enter`, `_register`), whose operations can open sockets without `socket`;
  - other processes' memory and descriptors (`ptrace`, `process_vm_*`, `pidfd_getfd`, `process_madvise`);
  - namespaces and mounts (`unshare`, `setns`, `clone` with any `CLONE_NEW*` flag, `mount`, the new mount API,
    `chroot`, `pivot_root`, file handles);
  - kernel subsystems a parser never needs (`bpf`, `perf_event_open`, `userfaultfd`, keyrings, module and kexec
    loading, swap, `reboot`, `acct`, `quotactl`, `personality`).

  `clone3` fails with `ENOSYS`, because its flags live in memory the filter cannot inspect; callers fall back to
  `clone`. The same filter restricts the sensitive-output scanner subprocess (`validation.md`).

The parent starts the child in new user, network, mount, IPC and UTS namespaces when the kernel allows it, and
falls back to seccomp alone otherwise. Either way no socket can be created. A 60-second wall-clock limit kills the
process group. The output is decoded strictly (unknown fields rejected), bounded, and checked against the snapshot's
paths and the contract's enums before it is stored. Each discovery records the isolation it actually had.

Process and network denial need Linux. On other systems (development only) the child gets the CPU limit alone and
results carry a `sandbox-degraded` warning.

## Dockerfiles

When a Dockerfile is present, a dedicated detector parses it statically (`build-execution.md`, ADR 0028). It reports
stages, base images, args, secret mounts (ID, target, required), contexts and frontend directive as evidence. The
build method is then suggested as `dockerfile`. The detector never executes instructions and never pulls images or
frontends. Variable references (`$VAR`) are never expanded; they are reported as ambiguous. Frontends are approved
only when listed, digest-pinned, in the organization's `frontends` policy (`recipes-and-policies.md`).

## Build readiness

Discovery also predicts what a build will need and where it is likely to break (ADR 0050), with the same static
rules: files are read as text, `RUN` lines and `package.json` scripts are split into words and separators, and nothing
is expanded, evaluated, executed or fetched. A word holding `$` predicts nothing.

- **Network needs** (`networkNeeds` on Dockerfile analyses and findings): package-manager commands map to egress
  presets, registry overrides and literal `curl`, `wget` and `git clone` URLs to exact `host:port` destinations, each
  with the file and line. `RUN --network=none` predicts nothing. Registry settings read from project files: `.npmrc`
  and Yarn Berry `.yarnrc.yml` (`npmRegistryServer`, top level and under `npmScopes`); `--index-url`,
  `--extra-index-url` and `--find-links` in requirements files; `index-url`, `extra-index-url` and `find-links` in a
  project `pip.conf` (`possible`); `url` of `[[tool.poetry.source]]`, `[[tool.pdm.source]]` and `[[tool.uv.index]]`
  and the `[tool.uv]` index keys in `pyproject.toml`, and `uv.toml`; literal URLs inside Gradle `repositories { }`
  blocks of `build.gradle(.kts)` and `settings.gradle(.kts)` in the context of a Dockerfile that runs Gradle without
  `--offline` (`internal/discovery/registries.go`). Only single-line TOML values are read.
- **Install-time downloaders**: `internal/discovery/downloaders-1.0.0.json` lists npm packages (by version range) and
  commands that download from fixed hosts, with the source of each entry. Lockfile versions are exact (`likely`);
  without a lockfile the lowest version of the manifest range is used (`possible`). `--ignore-scripts`, development
  dependencies left out by `--omit=dev` or `NODE_ENV=production`, and the skip variables an entry names suppress it.
  Root lifecycle scripts (`preinstall`, `install`, `postinstall`, `prepare`) count as part of an install; `npm run X`
  in a Dockerfile follows script `X`.
- **Linking.** A Dockerfile gets the install-time needs of the application in its build context (its own directory
  on the analysis page, the spec's context in admission) only when it installs that application's packages.
- **Readiness items** (`readiness`): deprecated npm flags, base images past or near end of life
  (`internal/discovery/runtime-eol-2026-10.json`), runtime constraints the base image tag excludes, a missing literal
  start file, rpm-based package managers no preset covers, and uncatalogued packages with install scripts. Each has a
  severity (`will-fail`, `may-fail`, `advice`), a fixer (`developer`, `administrator`), a confidence (`certain`,
  `likely`, `possible`) and a fix.

Discovery knows no policy. The controller compares the needs with the egress policy when it serves a discovery and
in admission (`build-execution.md#build-readiness`). The sandbox output check bounds these fields like the rest.

## Detector contract

Each detector returns:

```
{detector_id, detector_version, source_subpath, language, framework, application_kind,
 runtime_constraints, package_manager, suggested_commands, required_platforms,
 evidence[], confidence, warnings[]}
```

`evidence[]` items: `{kind, path, pointer (e.g. JSON pointer / line), observation, weight}`.

Deterministic manifest readers are the baseline. Required fixture coverage: Node.js, TypeScript, Python, PowerShell,
.NET. Go and Java plugins implement the same interface. Tests cover malformed/giant manifests, malicious archives,
polyglot trees and monorepos.

## Confidence rules

Confidence is a level derived from weighted evidence, **never an invented percentage**.

| Evidence weight | Examples |
|---|---|
| Strong (3) | Lockfile matching a manifest (`package-lock.json` + `package.json`); `*.csproj` with `TargetFramework`; `pyproject.toml` with build backend; explicit `engines`/`requires-python` |
| Moderate (2) | Manifest without lockfile; framework dependency declared; `scripts.start` present; `#requires -Version` in PowerShell |
| Weak (1) | File extensions only; README mentions; conventional directory names; port literals in source |
| Contradicting (−2) | Conflicting lockfiles (npm + yarn), multiple start candidates, conflicting runtime constraints |

Per finding: **high** if score ≥ 5 with at least one strong item and no contradiction; **medium** if score ≥ 3;
otherwise **low**. Any contradiction caps the level at medium and adds a warning. The UI auto-fills only high
findings, asks for confirmation on medium, and asks the question outright on low.

## Language notes

- **PowerShell**: inspect `.ps1`, `.psd1`, `.psm1`, `#requires`, `Import-Module`/`using module` hints. Warn on
  Windows-specific commands/modules (e.g. registry provider, WMI/CIM-only cmdlets, Windows paths). Static analysis
  cannot prove all runtime requirements; say so.
- **Node.js/TypeScript**: never assume HTTP; derive app kind from evidence (framework deps, `bin` field, scripts).
- **Monorepos**: workspace manifests (npm/yarn/pnpm workspaces, solution files, multiple `pyproject.toml`) produce
  multiple candidate apps; the user must choose.
- Ambiguous ports or start commands are flagged, never guessed.
- **Python web start command**: without a Procfile, a module-level `FastAPI(`, `Starlette(` or `Flask(` object in a
  conventional entry module (`main.py`, `app.py`, `asgi.py`, `wsgi.py`, `server.py`, `app/main.py`, `src/main.py`,
  `app/app.py`) gives `uvicorn <module>:<name> --host 0.0.0.0 --port 8000`, or `gunicorn --bind 0.0.0.0:8000
  <module>:<name>` for Flask, only when the server is a declared dependency (`TestPythonServerStart`).
- **Frontend and backend**: a Node.js static site and an HTTP service in separate folders are also offered together
  as one image behind Samuori's web launcher (ADR 0055).
