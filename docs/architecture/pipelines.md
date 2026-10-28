# Git pipelines

Status: **Implemented (WP28)** for HTTPS Git repositories with HMAC-signed or GitLab-token webhooks, manual runs and
API runs. Tested against `git http-backend`. Live GitHub, GitLab, Gitea and Forgejo deliveries are not yet verified.
Decision: [ADR 0033](../adr/0033-git-pipelines-and-webhooks.md). Product decision: D20. Threats: TM-70, TM-71.
Screen: **Pipelines**.

## What a pipeline is

A pipeline belongs to a project and names:

- a spec (its latest revision, or a fixed revision);
- an HTTPS repository URL;
- the refs it builds, as full ref globs such as `refs/heads/main` or `refs/tags/v*`;
- the webhook scheme;
- the identity it runs as (its **owner**);
- optionally, a read-only repository token and whether private network addresses are allowed.

A run does three things: it fetches the commit at one ref, stores the commit as a snapshot, and starts a build. A
pipeline has no steps of its own. Checks run as for any build. Publication happens only when the project and spec
ask for automatic publication, and approvals and the publish-time gate still apply
([publication](publication.md)). A pipeline run therefore ends at `started`; the build, its checks and its
publication keep their own lifecycles ([build lifecycle](build-lifecycle.md)).

## Who may do what

| Action | Who |
|---|---|
| Create, enable, disable, rotate the webhook secret | Organization administrators |
| See pipelines and runs | Administrators, and anyone who can see the project |
| Start a run by hand or through the API | Developers and maintainers of the project |

The owner is the creating administrator or a service identity of the organization, never another person. The owner
must hold the developer or maintainer role on the project when the pipeline is created. A service identity bound to
that one project is recommended. Each run reloads the owner's roles: if the owner is disabled, deleted or loses the
role, runs fail with `owner_unavailable` or `owner_forbidden`. A person who starts a run also needs a build role on
the project. The snapshot and the build are created through the same services and checks as an upload and a build
started in the UI (D29).

## Webhooks (SEC-HOOK-01, SEC-HOOK-02)

The endpoint is `POST /api/v1/hooks/pipelines/{pipelineId}`. It is public at the session layer and authenticated by
signature (`x-samuori-signed` in the contract).

| Scheme | Senders | Check |
|---|---|---|
| `hmac-sha256` | GitHub, Gitea, Forgejo and others | `X-Hub-Signature-256: sha256=<hex>` over the raw body, with `hmac.Equal` |
| `gitlab-token` | GitLab | `X-Gitlab-Token`, compared in constant time |

Samuori generates the 256-bit secret and shows it once, at creation or rotation. Rotation invalidates the old secret
immediately. Senders must use content type `application/json`.

Each delivery is handled in this order:

1. A per-pipeline rate limit applies (burst 30, then 1 per second), and bodies over 1 MiB are refused.
2. The signature is verified before the body is parsed. Unknown pipelines, disabled pipelines and failed checks all
   get the same `401`. A failed check on an existing pipeline is audited (`pipeline.hook`, `denied`).
3. Only push events start runs. Ping and other events are acknowledged and ignored.
4. From the payload, Samuori reads only `ref`, `after` or `checkout_sha`, and `deleted`. The ref must be a well-formed
   `refs/heads/` or `refs/tags/` name that matches the pipeline's patterns, and the commit must be 40 or 64 hex
   digits. Branch deletions are ignored. Repository URLs, commit messages and every other field are never read, so
   nothing in a payload becomes a URL, path, command or argument.
5. Replays are no-ops:
   - the digest of the signed body is unique per pipeline, whatever the run's state. Delivery ID headers are not
     signed, and no supported Git host signs a timestamp, so they are not used: a captured delivery replayed under a
     new delivery ID is still a duplicate. A host's "redeliver" therefore starts nothing; rerun with `samuori pipeline
     run` or the pipeline page's Run now button instead;
   - a commit that already has a queued, fetching or started run is recorded as `skipped`.
6. At most 20 runs may be queued or fetching per pipeline; above that the answer is `429`.

## Fetching

The controller fetches with go-git plumbing (ADR 0033):

- one upload-pack session over smart HTTP for the single ref, at depth 1 and without tags;
- every connection passes the address guard (SEC-NET-03): link-local and metadata addresses are always refused, and
  loopback and private addresses only when the pipeline allows them;
- redirects are refused;
- the downloaded pack and the archive are capped at the upload archive limit, and the file count at the upload file
  limit;
- objects go to a temporary directory that is removed afterwards. There is no worktree, checkout, hook, Git
  configuration, submodule, LFS or `git` process.

The commit's tree is written as a TAR. Only the executable bit of the mode is kept. Symbolic links are written as
links so that the snapshot entry rules refuse and count them, and submodules are skipped and counted
(`skippedEntries`). The archive then goes through `sources.Ingest`, with the same entry rules, exclusions, limits and
preflight warnings as an upload ([sources](sources.md)).

Fetch failures carry a code and a message that never include the token or URL credentials:

| Code | Meaning |
|---|---|
| `auth_failed` | The repository refused the token |
| `not_found` | The repository does not exist or the token cannot read it |
| `ref_not_found` | The ref does not exist |
| `blocked_address` | The repository resolved to an address the guard refuses |
| `redirect` | The server answered with a redirect |
| `too_large`, `too_many_files` | Over the upload limits |
| `timeout` | Fetching took more than 10 minutes |

## Processing

A loop on each controller claims one queued run at a time, using a conditional update with a 20-minute claim. A run
left in `fetching` by a crash is claimed again after that time. The build is created with the idempotency key
`pipeline-<run>`, so a second claim never creates a second build. A fetched commit that already has a started build
is `skipped`. Every outcome is audited (`pipeline.run`).

## External CI

External CI starts runs with `POST /pipelines/{id}/runs` and an API token of a service identity that has a build role
on the project. The run is then recorded with trigger `api`. Repeating a call with the same `Idempotency-Key`
returns the first run. Snapshot uploads and builds through the API remain available for CI that prefers to send
source itself.

## Not supported

SSH remotes, Git LFS, submodule contents, signed-tag or signed-commit verification, history beyond one commit
(`git describe`), and pull-request builds.
