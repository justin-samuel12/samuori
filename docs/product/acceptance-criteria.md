# Acceptance Criteria and Completion Definition

Status: **Authoritative.** Milestone-level criteria live in [`../roadmap.md`](../roadmap.md); per-decision status in
[`../traceability.md`](../traceability.md).

## Reliability and maintenance acceptance criteria

1. All-in-one on a single Linux host installs, runs local auth, receives an uploaded project, builds a Node.js API
   and publishes to a local OCI registry without optional enterprise services.
2. A beginner builds images using only a browser — no Dockerfile, CLI, or local container runtime.
3. Discovery safely detects Node.js, Python, PowerShell (including Windows-specific warnings) and .NET at minimum;
   polyglot fixtures produce explainable findings.
4. A generated spec survives a Form → YAML → Form round trip without semantic loss (ADR 0005 as amended).
5. Wrong build-engine/OS/architecture combinations fail **before scheduling** with an actionable message.
6. Queued jobs survive controller restart; worker failure creates no conflicting successful executions or duplicate
   publish events.
7. Two teams cannot read one another's restricted sources, secrets, logs, caches, artifacts or repositories, even
   through direct API calls.
8. Build, validation (test/scan) and publication outcomes remain distinct; policy-blocked and skipped checks are not
   represented as successes.
9. Retag/promotion is digest-aware and logged; destination digest verified; signatures and attestations preserved
   or explicitly revalidated.
10. Remote worker enrollment, revocation, certificate rotation and controller pinning are end-to-end tested.
11. SQLite → PostgreSQL migration validated on nontrivial data; a disaster-restore rehearsal proves backup scope and
    limitations.
12. Full HA deployments have shared database/messaging with explicit ownership and upgrade compatibility; federation
    tolerates disconnection without ambiguous job ownership.
13. Documentation includes admin installation, secure defaults, recovery, operator runbooks, contributor setup,
    release process and licensing inventory.

## Completion definition

The initiative is complete only when: all 31 decisions are implemented or documented as deferred **with owner
approval**; acceptance tests and security reviews pass; an independent all-in-one install works end-to-end;
distributed operation is validated; third-party license provenance is documented; and an operator can recover the
system from failure using the published runbooks.

## Mandatory exit checklist

- [ ] All 31 decisions trace to passing validation or to an explicitly approved, documented exception.
- [ ] Source is independently written; no prior-project or employer code or materials imported.
- [x] Outbound license chosen: Apache-2.0 (ADR 0056); third-party license review: `make license-check` and `THIRD_PARTY_NOTICES.txt`.
- [ ] All-in-one install builds/publishes a real Node.js project entirely via browser, to a local OCI registry, with no enterprise dependencies.
- [ ] Remote enrolled workers build and rotate/revoke identities; queues reconcile under failures.
- [ ] Administrator-managed ECR, secrets, storage, recipes, identities, RBAC and policies have real end-to-end authorization tests.
- [ ] Image digest, source snapshot, recipe, BuildSpec, scanner findings, signatures/attestations, approval and promotion chain are queryable and audit-backed.
- [ ] Production-supported topologies, HA/federation, backup, migration and upgrade workflows have documented test evidence.
- [ ] Security negative tests, license/attribution scans, SBOM, release signing, contribution policies and operator instructions are present.
- [ ] No general Backstage, application deployment, source hosting, arbitrary workflow automation or employer-specific dependency slipped into scope.

## Evidence rules

Each milestone provides reproducible commands and collected results. Never replace real test output with fabricated
success examples. Do not mark functionality production-ready without mandatory negative tests and recovery evidence.
