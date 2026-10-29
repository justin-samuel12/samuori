# The `samuori` command

Status: **Implemented (WP28; workflow commands and profiles 2026-10-28).** Product decision: D29. Source:
`cmd/samuori`, `internal/cli`.

`samuori` covers the developer workflow from a terminal or a CI job: sign in, create a project, upload source, see
what discovery found, write and save a spec, build, follow the log, download the image or deployment files, publish,
and ask administrators for access. It calls the same versioned API as the web UI with the generated client in
`internal/apiclient`, so every command passes the same server-side authorization, validation and execution paths.
It adds no privileges and no shortcuts. Installing and joining hosts is not part of it: that runs as root from the
release folder with `samuori-setup` and `samuori-join` (`high-availability.md#join`, ADR 0045).

## Install

Release builds put `samuori` next to `samuori-server` and `samuori-worker`. Copy it onto your `PATH`, then turn on
completion if you like:

```sh
samuori completion bash > ~/.local/share/bash-completion/completions/samuori   # or: zsh, fish
samuori help            # every command, grouped
samuori help spec       # one command's usage
samuori build --help    # the same, with its flags
```

## Sign in

Create a personal API token in **Account**, or, for CI, a token of a service identity that an administrator has
bound to the project with the developer or maintainer role. Then:

```sh
samuori login --url https://samuori.example.com            # prompts for the token without echoing it
printf '%s\n' "$TOKEN" | samuori login --profile ci --url https://samuori.example.com
samuori whoami
```

`login` checks the token with the server before it stores anything. A token is never accepted as a flag value, so
it does not end up in shell history or process lists, and no command prints it.

### Profiles

`login` stores a profile in `${XDG_CONFIG_HOME:-~/.config}/samuori/`: `config.json` holds each profile's URL and CA
bundle, and `tokens/NAME` holds its token. Both files are mode 0600 in a 0700 directory and are replaced atomically.
The last profile signed in to is the default; pick another with `--profile NAME` or `SAMUORI_PROFILE`.
`samuori profiles` lists them (`*` marks the default) and `samuori logout [--profile NAME]` deletes one with its
token. Logging out does not revoke the token on the server; revoke it in **Account**.

Settings are taken from flags first, then the environment, then the profile:

| Setting | Flag | Environment |
|---|---|---|
| Server | `--url https://samuori.example.com` | `SAMUORI_URL` |
| Token | `--token-file FILE` (mode 0600) | `SAMUORI_TOKEN_FILE`, then `SAMUORI_TOKEN` |
| Private CA | `--ca-file FILE` (PEM) | `SAMUORI_CA_FILE` |
| Profile | `--profile NAME` | `SAMUORI_PROFILE` |

A profile's token and CA bundle are used only when the server URL is the profile's own, so a token stored for one
server is never sent to a URL given by a flag or variable. CI needs no profile: setting `SAMUORI_URL` and a token
variable is enough. Token files that group or others can read are refused. Plain `http` is refused except to a
loopback server, redirects are not followed, and proxy environment variables are honoured.

## A first build

```sh
samuori projects create web --team platform          # maintainers and administrators
snap=$(samuori upload --project web .)
samuori analyze "$snap"                               # what discovery found, and what to fix first
samuori spec init web --snapshot "$snap"              # writes samuori.spec.yml
samuori spec validate samuori.spec.yml
samuori spec apply samuori.spec.yml --project web     # creates the spec, or a new revision when it changed
samuori spec check web --snapshot "$snap"             # admission dry run; creates no build
build=$(samuori build --spec web --snapshot "$snap")
samuori logs --follow "$build"
samuori status "$build"
samuori download "$build"                             # then: docker load -i <file>
samuori deploy-files compose --build "$build"         # a starting compose.yml; Samuori deploys nothing
```

Projects and specs are named by ID or by name. A name must match exactly one project the token can see; a spec name
is looked up in `--project`, or in every visible project when it is left out, and must match one spec.

Discovery is static: `analyze` and `spec init --snapshot` never run the uploaded code. With `--snapshot`, `spec init`
runs discovery if the snapshot has none, then writes the server's recommended suggestion
([Suggested specs](../architecture/buildspec.md#suggested-specs)), including built-in recipe specs; `--method` picks
the first suggestion for that method instead. It explains the choice, the warnings (such as an assumed port) and the
other suggestions on standard error. A `NAME` replaces the suggested `metadata.name`. Without `--snapshot`, or when
the server has no suggestion for the method, it writes a plain `dockerfile`, `railpack` or `buildpacks` starter
(`dockerfile` when the current directory has a `Dockerfile`). `spec apply` and `spec validate` take YAML or JSON (a document, or
`{"document": …}` as the API takes it) and validate on the server. `spec apply` adds no revision when the document is
unchanged.

Directory uploads apply the server's exclusion rules before transfer, from the shared `internal/excludes` package,
so version-control data, dependency directories and credential files never leave the machine. Symbolic links and
special files are skipped, as the server would refuse them. Only the executable bit of each file's mode is kept. The
server applies the same rules again.

## Commands

| Group | Commands |
|---|---|
| Sign in | `login`, `logout`, `profiles`, `whoami` |
| Projects and sources | `projects [list\|create\|show\|rename\|archive]`, `upload`, `analyze` |
| Build specs | `spec init\|list\|show\|validate\|check\|apply` |
| Builds | `build`, `builds [list]`, `status`, `logs [--follow]`, `cancel` |
| Images and publication | `images [list]`, `download`, `deploy-files compose\|helm`, `publish`, `promote` |
| Requests | `requests [list]`, `request repository\|base-image\|egress`, `withdraw` |
| Automation | `ci`, `pipeline run`, `pipelines [list]`, `api` |
| Administration | `config export [-o FILE]`, `config import FILE [--apply]` |
| Other | `completion`, `version`, `help` |

`samuori help` gives each command's usage. Notes:

- `projects archive` needs `--yes`: archiving revokes the project's grants, which restoring does not bring back.
- `download` saves the archive the server offers for `docker load` or `podman load`; the server decides who may
  download (maintainers, publishers and administrators, and developers when the publication policy allows it).
- `deploy-files` takes `--build` or `--artifact` (one image, optionally `--publication` for its reference) or
  `--project` (one file for all of the project's images).
- `promote --publication ID --destination ID` requests a publication of kind `promote` from a published image.
- `withdraw` takes the approval ID that `requests` shows first.
- `status` prints JSON, as it always has.
- `config export` and `config import` need an organization administrator's token
  ([configuration export](configuration-export.md)). `config export` writes YAML (JSON with `--json`) to standard
  output or `-o FILE` (mode 0600). `config import FILE` previews and changes nothing; it prints each item's change and
  reason, then what needs attention. `--apply` applies the plan with a fresh idempotency key and exits 1 if any item
  failed. An invalid file exits 3 and lists each problem by path.

## Output and scripting

Lists print aligned tables; commands that create something print its ID on standard output. Progress, notes and
errors go to standard error, so `id=$(samuori …)` captures only the result. `--json`, before or after the command,
prints the full API objects instead. Errors show the server's own message with the HTTP status, problem code and
request ID: `samuori: <step>: <message> (HTTP <status>, <code>), request <ID>`. There is no colour.

```sh
samuori --json builds --project web | jq -r '.[] | select(.state == "blocked") | .id'
```

### Any operation: `samuori api`

`samuori api OPERATION_ID` calls any operation of the OpenAPI contract built into the binary and prints the JSON
answer. `samuori api --list` lists every operation with its method and path.

```sh
samuori api getProject --path projectId=01J...
samuori api listBuilds --query state=blocked --query limit=10
samuori api createSpecRevision --path specId=01J... --header 'If-Match="3"' --body revision.json
echo '{"yaml":"…"}' | samuori api validateSpecDocument --body -
```

An `Idempotency-Key` is added when the operation takes one. Tokens need no CSRF header. A `--header` cannot replace
the token.

## Waiting and exit codes

With `--wait` (and always in `ci`):

- `build` succeeds only when the build succeeded **and** its checks passed;
- `publish` and `promote` succeed only when the image is published, and keep waiting while an approval is pending;
- `pipeline run` waits for the run to start a build, then waits for that build like `build --wait`.

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | The build, checks, publication, run, validation or admission did not succeed; blocked and skipped count as failures |
| 2 | Usage or configuration error |
| 3 | Request refused or server error (the message includes the server's code and request ID) |
| 4 | Timed out while waiting; the work continues on the server |

## CI

`samuori ci` runs the whole path in one step: upload `PATH` (default `.`), build the spec, wait for the build and its
checks, then optionally publish and save deployment files. It succeeds only when every requested step did, and
prints a summary (snapshot, build, image, publication, reference, files), as JSON with `--json`.

```sh
export SAMUORI_URL=https://samuori.example.com
export SAMUORI_TOKEN_FILE=/run/secrets/samuori-token
samuori ci --spec web --project web \
  --publish 01J...DESTINATION --tag "$GIT_SHA" \
  --deploy-files deploy/ .
```

`--deploy-files DIR` writes a `compose.yml` and a Helm chart that use the publication's reference, or the name
`docker load` gives the image when nothing was published. For specs kept in the repository, run
`samuori spec apply samuori.spec.yml --project web` before `samuori ci`.
