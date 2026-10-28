# 0029. Build secrets: logical IDs, project-scoped bindings and session-scoped delivery

Status: **Accepted as direction, with conditions** (2026-10-19, owner decision OD-13). The delivery granularity below
was verified by the OD-15 spike (`docs/spikes/buildkit-secrets.md`). The spike showed that the session boundary does not hold
on a shared daemon or with reused cache. The owner then approved OD-16 to OD-18 (2026-10-19):
- one build per daemon;
- a fresh daemon per secret-bearing build;
- no cache reuse for secret-bearing builds until OD-19 validates scoped controls (`docs/architecture/build-cache.md`). Design:
[`../architecture/secrets.md`](../architecture/secrets.md). Builds on ADR 0015 (providers).

## Context
Real builds need several text and binary secrets per step: passwords, certificate passwords, private keys,
certificates. Specs must stay portable, and secret values and provider paths must never appear in specs, run
records, plans, generated Dockerfiles, audit events or logs.

## Decision
- **Logical IDs in the spec.** The BuildSpec declares secret IDs with `required`, `type` (`text` | `binary`), usage
  (`build` | `test`) and an optional expected mount target. No provider paths, accounts, roles or values.
- **Bindings are admin records.** A project-scoped binding maps a logical ID to a provider profile and provider
  reference. It also states the permitted usage and the eligible pools or trust domains. Bindings are created or
  approved by authorized administrators, versioned, audited, rotatable and can be disabled.
- **Secret classes are separate:**
  - controller-only secrets (provider credentials, publication credentials, signing and CA keys, webhook and
    notification secrets), which never leave the controller;
  - build-time and test-time secrets, delivered to one authorized execution;
  - registry pull credentials, which are held by the mirror and never by build steps (ADR 0030).
- **Admission** resolves the union of spec declarations and statically detected Dockerfile mounts. It checks
  project permission, binding existence and status, usage policy and pool eligibility before scheduling. A missing
  required binding blocks admission with an actionable message. An unbound optional secret is simply not mounted.
- **Delivery through supported BuildKit APIs.** The plan records logical IDs and binding versions, never values.
  `samuori-worker` is the BuildKit client. Its provider returns "not found" only for spec-optional, unbound IDs and a
  hard error for anything else, because BuildKit treats mounts as optional by default. It registers a secrets
  provider on the build **session**, the supported BuildKit mechanism. BuildKit asks that provider for a secret by ID when it needs one.
- **Security boundary: one attempt, on a dedicated, freshly started daemon, with an isolated cache.** The session
  alone is not a boundary (OD-15 S7, S8). It is not the individual `RUN` step either. Samuori does not rely on BuildKit
  calling back once per step. The worker releases an ID only if it is in the plan, the lease is current and the
  controller authorizes the release. The controller reads the provider just in time and only after lease
  verification. Released values remain available to the build engine for the rest of that session.
- **Verified (OD-15):** BuildKit requests a secret each time a `RUN` that mounts it executes; cache hits request
  nothing; unused or out-of-target mounts are never requested; provider errors fail the build. **Not upheld:** in a
  shared daemon, identical concurrent steps from different sessions run once with one session's secret, and cached
  results are reused regardless of secret values. Values remain in daemon memory after use. Originally listed as
  must-verify: when BuildKit requests secrets (per mount, per step, or at solve
  start); whether cache hits skip the request; how long values stay in engine memory; and what happens to mounts and
  in-flight requests on cancellation. The documentation will state the boundary actually observed.
- Plaintext is kept out of persistent platform storage and logs. Worker and engine memory is not a protection
  against a compromised worker (see "Honest limits").
- **Cleanup.** Requests are refused once the lease ends, is revoked or the run is cancelled; the worker then ends the
  session and cancels the solve. The build environment is destroyed (`vm-per-job`) or scrubbed. Cleanup outcomes
  are recorded.
- **Audit** records every resolution and delivery decision (actor, run, attempt, logical ID, binding version,
  outcome), never values.
- **Honest limits.** A worker or build VM that is compromised during a build can read the secrets delivered to that
  build. Mitigations reduce exposure (one build per VM, short-lived delivery, narrow bindings); they do not
  prevent it.
- Temporary secret mounts (inputs) stay distinct from sensitive files the build generates (ADR 0031).

### Implementation (WP16, 2026-10-19)
Implemented as described in `docs/architecture/secrets.md#implementation-wp16` for the local provider and the
`rootless-host` profile. Building on the observed behavior, daemons run with BuildKit build history disabled, since
history retains raw step output.

## Consequences
- Developers never handle provider credentials or paths.
- Administrators manage bindings, policies and rotation centrally.
- Secret delivery depends on the worker channel being up for the build's whole duration.
