# Open-Source Integration Gates and Spikes

Status: **Authoritative.**

## Verification gate (required before any adapter is called "implemented")

1. Pinned upstream version and authoritative license/notice (recorded in [`../dependencies.md`](../dependencies.md)).
2. Real proof against the supported upstream CLI/API.
3. Explicit compatibility matrix: OS, architecture, deployment profile.
4. Isolation, network and credential threat analysis.
5. Automated functional, negative and version-upgrade tests.
6. Diagnostics and graceful degradation.
7. A replacement plan, or an intentional lock-in ADR.

A candidate is never described as a completed integration merely because it appears in a dependency list. If an
integration fails the requirement, document the evidence and select an alternate mature OSS component or write a
minimal, independently authored adapter.

## Required spikes (M0 unless noted)

Spikes run in disposable, self-funded environments, never against employer or production accounts. Record
reproducible commands and results in `docs/spikes/<name>.md`.

| Spike | Questions to answer with evidence |
|---|---|
| Railpack / Buildpacks | Is detection useful without executing source? Known: Railpack planning downloads and runs `mise`; CNB detect runs buildpacks. Test Railpack with pre-staged `mise` and denied egress; compare proposals with original manifest detectors. |
| BuildKit | Rootless operation (native Linux and in Compose); remote worker build; cache import/export; multi-platform manifest; secret mounts; SBOM/provenance attestations; resource limits; egress control. |
| Browser folder upload | Directory selection APIs across target browsers; chunked/resumable upload; limits; cancellation. |
| NATS | Controller crash after DB commit before dispatch; consumer redelivery; worker partition; lease expiry and fencing; restart behavior. |
| Image retention/copy | Copy by digest with ORAS/Skopeo/go-containerregistry; referrers (signatures/SBOM/attestations) portability. |
| Windows workers | Engine capability and isolation feasibility before Windows is enabled as a target. |
| ECR (M9) | Scoped role discovery/authorization; lifecycle capability; cross-account/region publish; immutable/protected tag behavior; digest verification. |
| Harbor / generic OCI (M6) | Push, catalog permissions, signature and attached-artifact behavior, copy/promote. |
| Storage (M5/M9) | Encrypted snapshots, GC, rollback-safe uploads, artifact availability across worker restart. |
