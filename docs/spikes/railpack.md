# Spike: Railpack Planning in a Sandbox

Date: 2026-10-19. Status: **Complete; consistent with ADR 0028 and `discovery.md`. One placement recommendation.**
Code: `tools/spikes/railpack/`. Raw output is kept in the maintainer's private records.

## Setup
- Railpack 0.42.0 in a disposable Ubuntu 24.04 VM, with synthetic Node and Python projects.
- The Node project has a `preinstall` script, and the Python project a `setup.py`; each writes a marker file if
  executed.
- Networked runs were traced with `strace` (process executions and connections). Sandboxed runs used bubblewrap
  0.9.0: no network, read-only root, read-only source, private `/tmp`, and a disposable writable copy of the cache
  Railpack had staged.

## Results

| Question | Observed |
|---|---|
| Does planning execute the project's code or package scripts? | **No.** No `preinstall` or `setup.py` marker in any run; nothing was executed from the source directory |
| What does it execute? | A `mise` binary that Railpack downloads (`mise-2026.10.8`, 140 MB). For Python, mise also runs pyenv's `python-build`, plus `bash`, `git`, `awk`, `sed` and similar |
| Network use when allowed | GitHub (including release-asset hosts) for Node and Python; Cloudflare-hosted endpoints for Node |
| Offline in the sandbox with the staged cache | **Works.** Node and Python plans were identical to the networked runs |
| Failure signaling | **`railpack plan` exits 0 and emits a near-empty plan when it fails** (seen with a read-only cache: "Failed to create mutex"). Only `railpack info` reported the error |
| Cache needs | Railpack needs write access to its `mise` directory (lock files); a disposable copy works |
| Plans produced | Node: `packages:mise, install, build, packages:apt:runtime`, start `npm run start`. Python: `packages:mise, install, build`, start `python main.py` |

## Conclusions
- Planning does not run uploaded code, which confirms `discovery.md`. It does run third-party binaries (mise, and
  pyenv scripts for Python) fetched at runtime. That is why Railpack stays **disabled by default** and is never part
  of the safe discovery baseline.
- Railpack can be used offline when Samuori supplies the mise cache. That cache must be built from pinned versions in a
  controlled job and verified by digest. It must never be fetched per request.
- Samuori must check the plan's content, not the exit code: an empty or partial plan is treated as a failure.
- **Placement recommendation:** because planning executes third-party binaries, run Railpack (when enabled) inside
  the build isolation environment (ADR 0027), not in the controller-side discovery subprocess (ADR 0024). The
  original static detectors stay in the discovery subprocess. No decision is needed until someone proposes enabling
  Railpack.
- In this VM, unprivileged bubblewrap couldn't set up a network namespace because of Ubuntu's AppArmor userns
  restriction, so the sandboxed runs used root without a user namespace. Inside an isolation VM that is acceptable;
  on a shared host it is not.
