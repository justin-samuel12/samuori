# 0012. Node roles, process model and configuration precedence

Status: Accepted (2026-10-19)

## Context
D06 requires one product release configured as controller, worker or all-in-one. The original directive also listed
separate `samuori-server`/`samuori-worker` binaries and required that the all-in-one local worker remain a separately
isolated subsystem, never running untrusted image assembly inside the API process.

## Decision
- One release (same version, same artifacts) contains `samuori-server`, `samuori-worker` and the optional `samuori` CLI.
- `samuori-server --role=controller|all-in-one`. In `all-in-one`, the controller also runs embedded NATS and the local
  artifact store, and expects one local worker.
- The local worker is always a separate `samuori-worker` process under a separate unprivileged OS user (native: separate
  systemd unit; Compose: separate container). It enrolls through the same protocol as a remote worker, using a
  local bootstrap token, so all-in-one exercises the real worker path.
- Only `samuori-worker` can reach the BuildKit socket. `samuori-server` never has engine access.
- Configuration precedence (highest first): flags → environment (`SAMUORI_*`) → config file → admin-UI values stored in
  the DB → built-in defaults. Values fixed by a higher source are read-only in the UI and labeled with their source.

## Consequences
No privileged path from the API process to build execution. All-in-one and distributed deployments share code paths,
so the vertical slice tests the distributed protocol early.
