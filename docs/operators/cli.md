# The `samuori` command

Status: **Implemented (WP28).** Product decision: D29. Source: `cmd/samuori`, `internal/cli`.

`samuori` calls the same versioned API as the web UI, using the generated client in `internal/apiclient`. Every command
goes through the same server-side authorization, validation and execution paths as the UI. The CLI adds no
privileges and no shortcuts. It is meant for external CI and for developers who prefer a terminal. Release builds
include it next to `samuori-server` and `samuori-worker`. Installing and joining hosts is not part of it: that runs as root
from the release folder with `samuori-setup` and `samuori-join` (`high-availability.md#join`, ADR 0045).

## Authentication

`samuori` uses an API token: a personal token from **Account**, or, for CI, a token of a service identity that an
administrator has bound to the project with the developer or maintainer role.

| Setting | Flag | Environment |
|---|---|---|
| Server | `--url https://samuori.example` | `SAMUORI_URL` |
| Token | `--token-file FILE` (mode 0600) | `SAMUORI_TOKEN` or `SAMUORI_TOKEN_FILE` |
| Private CA | `--ca-file FILE` (PEM) | `SAMUORI_CA_FILE` |

The token is never accepted as a flag, so it does not end up in shell history or process lists. A token file that
group or others can read is refused. Plain `http` is refused except to a loopback server, so the token never
crosses a network in clear text. Redirects are not followed, so the token is never sent to another host. Proxy
environment variables are honoured.

## Commands

| Command | Does |
|---|---|
| `samuori whoami` | Shows the identity and roles behind the token |
| `samuori projects` | Lists projects the token can see (`ID<TAB>name`) |
| `samuori upload --project ID [--exclude PATTERN]... PATH` | Uploads a directory or a TAR, TAR.GZ or ZIP archive, waits for the snapshot and prints its ID |
| `samuori build --spec ID (--snapshot ID \| --source PATH) [--revision N] [--clean] [--wait] [--timeout 1h]` | Starts a build of the spec's latest revision (or `--revision`) and prints the build ID |
| `samuori status BUILD` | Prints the build as JSON |
| `samuori logs BUILD` | Prints the build log |
| `samuori publish (--artifact ID \| --build ID) --destination ID [--tag T]... [--wait]` | Requests a publication; approvals and the publish-time gate apply |
| `samuori pipeline run PIPELINE --ref REF [--wait]` | Starts a pipeline run; `main` means `refs/heads/main` |
| `samuori version` | Prints the client version |

`--json` before the command prints full JSON results instead of IDs. Progress goes to standard error, and results go
to standard output.

Directory uploads apply the server's exclusion rules before transfer, from the shared `internal/excludes` package, so
version-control data, dependency directories and credential files never leave the machine. Symbolic links and
special files are skipped, as the server would refuse them. Only the executable bit of each file's mode is kept. The
server applies the same rules again.

## Waiting and exit codes

With `--wait`:

- `build` succeeds only when the build succeeded **and** its checks passed;
- `publish` succeeds only when the image is published, and keeps waiting while an approval is pending;
- `pipeline run` waits for the run to start a build, then waits for that build like `build --wait`.

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | The build, checks, publication or run did not succeed; blocked and skipped count as failures |
| 2 | Usage or configuration error |
| 3 | Request refused or server error (the message includes the server's code and request ID) |
| 4 | Timed out while waiting; the work continues on the server |

## Example CI step

```sh
export SAMUORI_URL=https://samuori.example
export SAMUORI_TOKEN_FILE=/run/secrets/samuori-token
samuori build --spec 01J9... --source . --wait
```
