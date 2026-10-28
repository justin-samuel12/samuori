# 0033. Git-triggered image pipelines and inbound webhooks

Status: **Accepted as direction** (2026-10-20, within product decision D20 under the owner's WP28 directive).
Design: [`../architecture/pipelines.md`](../architecture/pipelines.md). Threats: TM-70, TM-71.

## Context
D20 asks for Git-driven build triggers and focused image pipelines, without becoming a generic workflow engine. The
existing lifecycle already runs build, checks and (for projects and specs that ask for it) automatic publication
with approvals. Missing are a source that is a Git repository rather than an upload, and a trigger. The repository
content is untrusted, as is anything sent to an unauthenticated webhook endpoint.

## Decision
- A **pipeline** belongs to a project. It names a spec, an HTTPS repository URL, ref patterns, an owner, and optionally
  a controller-only access token. It has no steps of its own. A pipeline run fetches one commit into a snapshot and
  starts a build. Checks, approvals and publication then follow the project and spec exactly as for an upload.
- Runs are started by a verified webhook, by a person in the UI, or through the API (external CI with an API token).
  Every path goes through the same service and authorization: a run builds with the **owner's current roles**,
  reloaded at run time, so a pipeline never grants more than its owner holds and stops working if the owner loses
  access.
- Webhooks are verified before anything else. Two schemes are supported: HMAC-SHA256 over the raw body
  (`X-Hub-Signature-256`, sent by GitHub, Gitea, Forgejo and others), and the GitLab shared token
  (`X-Gitlab-Token`), compared in constant time. Samuori generates the secret and shows it once. Bodies are limited to
  1 MiB. Delivery IDs are recorded once per pipeline, and a commit that already has a run is skipped, so a replayed
  delivery is a no-op. An unknown pipeline and a bad signature get the same answer.
- The payload only selects a ref and names a commit. Both are validated against fixed patterns and the pipeline's
  ref patterns. Nothing in the payload is used as a URL, a path, a command or an argument (TM-71).
- Fetching is done in process with **go-git plumbing** (Apache-2.0): one upload-pack session over Samuori's guarded HTTP
  client, depth 1, a single ref, and no tags. There is no worktree, checkout, hook, Git config, submodule, LFS or
  `git` binary. Only HTTPS is accepted. The address guard (SEC-NET-03) applies at connect time, redirects are
  refused, and the pack stream is capped at the upload archive limit.
- The fetched tree is written as a TAR and passes the same entry rules, exclusions, limits and preflight as an
  upload. For example, symbolic links and submodule entries are refused and counted.

## Consequences
- Pipelines add no execution path. Untrusted repository content reaches only the existing snapshot and build
  isolation.
- SSH remotes, Git LFS, submodules and tag-signature verification are not supported.
- Repositories that need history (`git describe`) get a shallow commit only.
- go-git brings ten small permissively licensed transitive modules (`docs/dependencies.md`).
