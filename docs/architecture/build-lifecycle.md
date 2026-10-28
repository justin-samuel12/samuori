# Lifecycle Models: BuildRun (with BuildAttempts), ValidationRun, Publication

Status: **Authoritative** (ADR 0003). These replace the two conflicting single state machines in the original
directive. Implement transitions only through the domain layer, inside DB transactions with an outbox record.

## Relationships

```
SpecRevision ─┐
SourceSnapshot├─> BuildRun 1──* BuildAttempt
RecipeVersion ┘       │ (succeeded)
                      v
                 ImageArtifact (identity = OCI digest, held in internal artifact storage)
                      │ 1
                      ├──* ValidationRun 1──* CheckResult
                      │        (initial run auto-created; rescans/revalidation create new runs)
                      └──* Publication 0..*──* Approval
                               (kind = publish | promote; promote has a source Publication)
```

- A **BuildRun** produces at most one ImageArtifact (multi-platform builds produce one index digest).
- An **ImageArtifact** is identified by digest; it is never mutated.
- A **ValidationRun** targets an ImageArtifact digest, not a BuildRun, so rescans and promotion revalidation reuse
  the same model. The initial run is created automatically when its BuildRun succeeds.
- A **Publication** targets an ImageArtifact digest and a destination repository. Promotion is a Publication with
  `kind=promote` and a `source_publication_id`; it never rebuilds.
- **Readiness** is derived, not a stored state: an artifact is *publishable to destination X* when the policy for X
  is satisfied by its latest applicable ValidationRun verdict plus any valid exceptions, evaluated at publish time.

## BuildRun

```
draft ─> pending_admission ─> queued ─> assigned ─> preparing ─> building ─> committing ─> succeeded
                                  ^                                                │
                                  └──── retry (infrastructure failure, new attempt)┘

draft ─> discarded
pending_admission ─> blocked
queued ─> expired
any non-terminal state after draft ─> cancelled
assigned | preparing | building | committing ─> failed | timed_out | blocked
```

| State | Meaning |
|---|---|
| `draft` | Spec revision saved; not submitted |
| `pending_admission` | Schema, semantic, policy and platform-compatibility checks; immutable build plan resolved and its digest recorded (ADR 0014) |
| `queued` | Admitted; waiting for a matching worker |
| `assigned` | A BuildAttempt holds a fenced lease |
| `preparing` | Worker fetching source snapshot and plan; isolated environment setup |
| `building` | Engine executing |
| `committing` | Uploading/verifying artifacts in internal storage |
| `succeeded` | ImageArtifact recorded by digest |
| `blocked` (terminal) | Any policy denial, at admission or later (e.g. a policy enforced during the build). Unsupported engine/OS/arch is always caught at admission, never after scheduling |
| `failed` (terminal) | Application build failure or exhausted infrastructure retries; `failure_class` = `application` \| `infrastructure`, with evidence. Policy denial is never `failed` |
| `timed_out` (terminal) | Exceeded run time limit |
| `cancelled` (terminal) | User/operator cancellation (actor recorded) |
| `expired` (terminal) | Queue TTL elapsed before assignment |
| `discarded` (terminal) | Draft deleted/expired by retention |

Retries create new **BuildAttempts**; the run returns from `assigned/preparing/building` to `queued` only for an
infrastructure failure with evidence (worker loss, lease expiry). Never reclassify an application failure as
transient without evidence.

### Failure classes

A plan the worker's release cannot render is version skew between controller and worker, so it is an
`infrastructure` failure (retried, then failed as the platform's), never `application`. The evidence is a recipe,
custom-recipe step kind or build method the worker does not know, or plan fields its release does not decode. Any
other render failure stays `application`.

## BuildAttempt

`leased → preparing → building → committing → completed | failed | abandoned | superseded`

Records `worker_id`, location, lease token, **fencing generation**, lease deadline, heartbeats, engine version,
isolation profile, timestamps, logs/metrics references and results.

### Worker leases

- Assignment is a compare-and-swap on the BuildRun row that increments the **fencing generation** and records the
  lease holder and deadline in the same transaction as the outbox message.
- Workers renew leases by heartbeat. A dispatched message that is not acknowledged within its visibility timeout
  is redelivered, and an attempt whose lease deadline passes is treated as lost; every worker request (renew, upload, state report) carries the attempt ID and
  fencing generation.
- The controller rejects any request whose generation is not current. The attempt becomes `superseded` and its
  partial artifacts are left for GC; a late worker can never overwrite a successor's outcome.
- When a lease expires, reconciliation marks the attempt `abandoned` and requeues the run (infrastructure failure
  with evidence) or fails it once the retry budget is exhausted.
- Artifact commits are keyed by attempt ID and generation, so a redelivered or replayed message is idempotent.

Details of the worker protocol: [`scheduler-and-workers.md`](scheduler-and-workers.md).

### Live logs

While an attempt builds, a worker that negotiated protocol 1.7 sends BuildKit's output to the controller every two
seconds (`AppendLog`), bound to the attempt's lease like every other call. The controller accepts output only from
the worker holding the current lease and generation, in order, in chunks of at most 64 KiB and at most 2 MiB per
attempt, and stores it in `build_live_logs` (migration 00064). `GET /builds/{id}/logs` returns it under
`=== attempt N (running) ===` with `X-Samuori-Log-Live: true`, and the build page reads it again every three seconds
while the run is not finished.

Builds that use secrets get no live output: their log is masked for delivered secret values only when the attempt
ends, so the worker does not send it, the isolation manager does not pass it on, and the controller refuses it; the
page says so instead. The final masked log is uploaded as before and replaces the live view; reconciliation deletes
live output once its attempt has ended, and anything older than a day. Live output is untrusted build output,
shown as text only.

## ValidationRun

```
pending ─> running ─> completed (verdict: passed | failed | blocked | incomplete)
              └─> cancelled | timed_out | errored
```

Each **CheckResult** has a category (`test` — startup/health/smoke/project tests; `scan` — vulnerabilities, secrets,
licenses; `sbom`; `provenance`; `signature`), `required` flag (from admin policy or project), and an outcome:

| Outcome | Meaning | Counts as success? |
|---|---|---|
| `passed` | Check ran and met its criteria | Yes |
| `failed` | Check ran and did not meet criteria | No |
| `blocked` | Policy prevented the check or its result violates a gate | No |
| `skipped` | Not run (with reason: optional and not selected, not applicable, dependency failed) | Never shown as passed |
| `errored` | Check could not complete (scanner unavailable, timeout) | No |

Verdict: `passed` only if every required check `passed` or has a valid, unexpired, scoped exception; `blocked` if any
gate denies; `failed` if any required check failed; `incomplete` if any required check skipped/errored. Scan results
record scanner name, version and vulnerability-DB timestamp. Runtime tests execute in a sandbox with no
privilege escalation and no metadata-service access.

Implementation: `docs/architecture/validation.md` (WP20).

Signing and provenance are produced by the controller at publication, per destination, after the push is verified.
They are stored under Cosign's tags and, where the registry supports them, as OCI 1.1 referrers, then read back and
verified. Promotion requires a verified source signature and carries the source's signatures with the image
(`signing.md`, ADR 0021, OD-25). They are not ValidationRun checks, so a validation verdict never depends on keys.

## Publication

```
requested ─> awaiting_approval ─> approved ─> gate_check ─> publishing ─> verifying ─> published
     │              │                             │              │            │
     │              └─> rejected | expired        └─> blocked    └─> failed ──┘   (retry = new attempt, idempotent)
     └─> cancelled                                                              published ─> retired
```

| State | Meaning |
|---|---|
| `requested` | Manual request or automatic publish per project mode (admin policy overrides) |
| `awaiting_approval` | Policy requires approvals; Approval objects are immutable (actor, reason, scope, expiry) |
| `gate_check` | Policy re-evaluated **at publish time** (tags, protected tags, latest ValidationRun, exceptions) to avoid TOCTOU |
| `publishing` | Copy by digest to destination; tags applied |
| `verifying` | Destination manifest/config/layer digests and attached signatures/attestations verified |
| `published` | Recorded with destination digest and tags; audit event emitted exactly once (idempotency key) |
| `retired` | Retired reference: no promotion or rollback from it; Samuori keeps the record and digest and does not delete registry tags. Deprecation is a separate mark on a `published` publication |
| `rejected`, `expired`, `blocked`, `failed`, `cancelled` | Terminal non-success outcomes, distinguishable in UI and API |

Publication retries are idempotent: the same idempotency key never produces duplicate publish events.

## Mapping from superseded definitions

| Original state | New location |
|---|---|
| `draft`, `queued`, `assigned`, `preparing`, `building` | BuildRun (same names) |
| `pending_validation` | BuildRun `pending_admission` (it was admission, not image validation) |
| `validating` | ValidationRun `running` (test checks) |
| `security_check` | ValidationRun `running` (scan/SBOM/signature checks) |
| `ready` | Derived publishability condition, not a state |
| `publishing`, `published` | Publication |
| `failed`, `cancelled`, `timed_out`, `blocked`, `expired` | Retained as terminal states in the appropriate model |

The UI presents all three as one timeline with separate outcomes (queued/building/testing/scanning/signing/publishing).
