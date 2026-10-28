# Registries, Images and Artifact Lifecycle

Status: **Authoritative.**

## Artifact store vs publication registry

Two distinct logical concerns:

- **Artifact storage** (internal, admin-managed): source snapshots, build logs, archives, SBOMs, built images before
  publication. Backends: local filesystem, NFS, EFS, S3, S3-compatible, OCI. Developers never select or override it.
  Routing policies consider artifact class, region/location, retention, project classification, quota and
  available capacity (D04).
- **Publication registries**: OCI-compatible registries where published images live (generic OCI Distribution,
  ECR, optional Harbor).

## Publication and promotion

- Build once; publish/promote by digest without rebuilding, subject to policy revalidation (ADR 0003).
- Retain digest identity where supported; verify destination manifest, config and layers; document digest
  variation when a manifest transformation is unavoidable.
- Moving an image with Skopeo/ORAS does not automatically carry signatures, SBOMs and attestations. Test and record
  their handling explicitly; copy referrers and re-verify.
- A build may finish with an immutable artifact in internal storage and be published later.

## Tags (D02)

- An immutable unique build reference tag is always generated.
- Each successful build gets a version, `<UTC date it succeeded>.<n>` counted per project (ADR 0054). A publication
  without tags uses the spec's `outputs.image.tags`, default `["{version}"]`; saving a spec warns when one of its
  fixed tags is already published by the project.
- Optional version/custom/semantic/moving tags, via templates and central rules.
- Protected tags, collision handling and approvals are enforced **at publish time** (Publication `gate_check`) to
  avoid TOCTOU.
- Signed image identity is the digest; tags are mutable pointers unless policy enforces immutability.
- Tag rollback is not a rollback of any deployed workload.

## Authentication

Pull and push credentials, mirrors and ECR auth methods: [`registry-auth.md`](registry-auth.md) (ADR 0030).

## ECR (D03)

- Admins discover existing repositories with scoped AWS SDK calls and optionally create/configure repositories where
  IAM allows. AWS IAM remains authoritative.
- Record AWS account and region; never trust a UI-selected repository URL blindly.
- Repository creation/configuration is admin-only; developers request repositories via an audited approval flow.
- Mutability, scanning and lifecycle are provider-specific: surface actual capabilities; mark unavailable actions.
- Prefer workload identity / temporary STS credentials.

## Generic OCI and Harbor

Credentials are resolved by admin-managed secret provider references. Harbor is optional; no vendor lock-in.

## Internal artifact storage (WP18)

- Local filesystem backend: the content-addressed blob store under `<dataDir>/blobs`, shared with source
  snapshots. See "Storage backends" for object storage, NFS and EFS.
- Workers fetch source only through the lease-bound `GetSource` stream: the canonical manifest first (its digest
  must equal the plan's snapshot digest), then every file, each checked against the manifest before the build sees
  it. A worker can fetch only the source of the attempt it currently leases (SEC-STO-01).
- Uploads go to `PUT /v1/artifacts` on the worker endpoint. They need the worker's mTLS identity and a grant from
  `GrantArtifactUpload`: an HMAC-signed token, valid 15 minutes, bound to one worker, attempt, fencing generation,
  kind (`image`, `log`, `cache`), digest and size. The controller re-checks the lease, stores the bytes in the blob
  store and rejects them unless they hash to the granted digest with the granted size.
- A run becomes `succeeded` only when the reported image digest is committed for that attempt and the stored bytes
  verify; otherwise the run fails (SEC-STO-02).

## Storage backends

Implemented in WP27 (D04). Every blob belongs to a **data class**, and each class is routed to one backend:

| Class | Holds | Route key |
|---|---|---|
| `source` | Snapshot files and manifests | `storage.routes.source` |
| `image` | Built image blobs | `storage.routes.image` |
| `report` | SBOMs, validation evidence and other reports | `storage.routes.report` |
| `other` | Anything else | follows the default |

A route is `local`, `s3` or `oci`. Without a route, a class uses S3 when a bucket is configured, else OCI when a
repository is configured, else local. A route to a backend that is not configured is refused at startup.
Developers never choose internal storage. The Artifact storage page shows each class's backend.

- **Local.** `<dataDir>/blobs` is the only copy.
- **S3 and S3-compatible object storage.** Set `storage.s3.bucket` and its region; also the endpoint and path-style
  addressing for S3-compatible services, and credentials as for other AWS adapters (workload identity, an assumed
  role with an external ID, or an access-key file). Keys are `<prefix>/sha256/<64 hex>`. Enable versioning or object
  lock on the bucket for protection Samuori does not provide.
- **OCI registry.** Set `storage.oci.repository` (`host[:port]/path`) and, if the registry needs them, a
  `USERNAME:PASSWORD` credentials file (mode 0600):
  - each blob is pushed with a small OCI 1.1 artifact manifest (`artifactType` `application/vnd.samuori.blob.v1`) that
    references it, records when it was stored, and is tagged `sha256-<hex>`. The registry's own garbage collection
    therefore never removes a stored blob, and the tag list enumerates the store;
  - deleting a blob deletes its manifest, and the registry reclaims the blob in its own garbage collection. The
    registry must allow deletes;
  - connections pass the address guard (SEC-NET-03) and never follow redirects. Plain HTTP is allowed only to a
    loopback registry.

For a class routed to S3 or OCI, the remote is the **durable copy**:

- Every write is stored remotely before it counts as stored. A write that cannot be stored remotely fails.
- The data directory is a cache. A missing local copy is fetched from a remote that holds it and **verified against
  its digest before it is used**. A corrupted remote copy is refused and never cached, and another remote may supply
  a good copy (TM-17).
- Local copies older than `storage.cacheEvictAfter` (default 24 h) are removed, but only once a remote is confirmed to
  hold them. Blobs of local classes are never evicted.

Garbage collection walks the local directory and every remote, and deletes from all of them. Tag listings carry no
times, so a blob held only by the OCI backend gets its time from its manifest. A blob whose time cannot be
established is kept, never deleted.

The controller checks every configured remote at startup and reports `object-storage` (S3) and `registry-storage`
(OCI) in health. **NFS and EFS:** mount them at the data directory or at `<dataDir>/blobs`. The store needs atomic
`rename` and `fsync`; NFSv4 and EFS provide both. The `artifact-storage` health check writes and removes a probe file
there.

**Not yet implemented:** per-location backends, capacity- or retention-based routing, and changing routes in the UI.
Routes are set in the controller configuration. Changing a route affects new writes; existing blobs stay where they
are and remain readable while their backend stays configured.

Evidence:

- Unit tests with in-memory object storage: uploads, refilling the cache, a corrupted remote copy refused, a failed
  upload failing the write, eviction only of copies a remote holds, routing by class, fallback past a corrupted copy,
  and times for listings without them.
- The garbage-collection test keeps a remote blob of unknown age.
- The OCI backend is tested against an in-process registry, and against CNCF Distribution (deletes enabled, then
  basic authentication) in the dev VM.
- A controller started in the dev VM with images and reports routed to Distribution and sources kept local reports
  `registry-storage: ok` and logs each class's route.
- The S3 adapter is tested against moto 5.2.3 in the dev VM.

## Garbage collection

Content-addressed artifact GC is documented and coordinated with active leases, in-flight publications and
promotions, and legal/audit holds. Deletion never removes something referenced by a hold.

Implemented as mark and sweep every 6 hours: kept are active holds, recorded artifacts, run artifact digests,
snapshot manifests and every file they list. Artifacts of attempts that did not complete are dropped after 7 days
unless held. Blobs written in the last hour are never deleted, which protects uploads in flight. Images of
publications and stored SBOMs are kept as well.

## Downloading and deleting (ADR 0053)

- **Download** (`GET /artifacts/{id}/download`): a validated image's OCI archive plus Docker's `manifest.json`, which
  tags a single-platform image `<project>/<app>:<build version>` (`samuori-<digest>` without a version), for `docker load`
  (classic and containerd image stores) or `podman load` (`TestDockerLoadable`, run with `docker load` on Docker 29.9.2),
  for project maintainers, publishers and administrators, and developers when the `publication` policy sets
  `developerDownloads` (off by default); refused for images that did not pass, are restricted or
  are quarantined; audited as `image.download`. The build page and the Images page offer it.
- **Delete** (`POST /builds/{id}/deletion`): project maintainers and administrators remove a finished build's image,
  logs and cache; refused while an active publication, an unreleased hold or a running validation uses the image.
  The record, attempts, validations and audit trail stay and the build page shows the deletion. Garbage collection
  then skips the build's artifacts and its failed publications' images, so the space is freed at the next run unless
  another build shares the content.

## Purging a project (ADR 0058)

An organization administrator purges an archived project (`POST /projects/{id}/purge`, with the project's name in
`X-Samuori-Confirm`). It marks every upload and build of the project deleted, removes cache entries, live output,
staged chunks, drafts, discovery results and conversion file contents, then deletes the snapshot manifests and files,
images, logs, reports and validation outputs that nothing else references; holds and content shared with other
projects are kept. Records with their IDs, digests and times and the audit trail stay. Images already pushed to
external registries are not deleted. A purge that fails part-way answers 500 `purge_incomplete` and can be repeated;
garbage collection no longer keeps a purged project's validation outputs either.

## Publication (WP21)

Implemented for generic OCI registries: [`publication.md`](publication.md).
