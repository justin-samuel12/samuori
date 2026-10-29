# Publication

Status: **Implemented for generic OCI registries (WP21) and Amazon ECR (WP27, `registry-auth.md`), with promotion, signing and restricted distribution
(WP25).** Lifecycle: [`build-lifecycle.md`](build-lifecycle.md)
(ADR 0003). Credentials: [`registry-auth.md`](registry-auth.md) (ADR 0030). Controller-only publication: ADR 0024.
Threat rows: TM-59, TM-61, TM-62, TM-63, TM-64.

Publication copies a validated image from internal storage to an admin-defined destination, by digest and without
rebuilding. Everything runs on the controller. Workers never see destinations, registries or credentials.

## Administration

| Record | Fields | Notes |
|---|---|---|
| Registry | name, kind (`oci`; ECR and Harbor in WP27), endpoint `host[:port]`, `plainHttp`, `allowPrivateNetwork` | Endpoints have no scheme, path or credentials. Every connection, including redirects and token services, passes an address guard: link-local and metadata addresses are always refused, while loopback and private addresses need `allowPrivateNetwork` (SEC-NET-03). Proxy environment variables are ignored |
| Credential profile | registry, type (`anonymous`, `oci-basic`, `oci-token`), purpose (`push`, `pull`), username, secret, repository scopes | The secret is a local secret (`secrets.md`). Using it in a profile makes it **controller-only**: it can never be bound to a project, and a bound secret cannot become a credential (SEC-WRK-08). **Test** checks connectivity and authentication without publishing; failures are stored with credentials redacted |
| Editing and deleting | registries and credential profiles | Administrators edit and delete both; deletes are refused while anything refers to them, and an edit never sends a stored secret to a different endpoint (`registry-auth.md#editing-and-deleting`) |
| Destination | registry, repository, push profile, allowed projects, protected tag patterns, immutable tags, accepts restricted, sign images, attach provenance, promotion only | Only listed projects may publish. The profile must be a push profile for the same registry whose scopes include the repository. A promotion-only destination accepts images only by promotion from another destination |

Developers see only the destinations their project may use (`GET /projects/{id}/destinations`): name and
repository, never registry details or credentials.

## Lifecycle

`requested → gate_check → publishing → verifying → published`, or `blocked` / `failed`. Destinations that require
approval add `awaiting_approval → approved` (or `rejected` / `expired`) before the gate (`approvals.md`).

**Promotion** (`POST /publications` with `kind: promote` and `sourcePublicationId`) copies an already published image
to another destination by the same digest, for example from staging to production. The source must be `published`
and the target must differ from it. The request starts a new ValidationRun (trigger `promotion`) against current
policy, and the promotion waits in `requested` until that run completes; its gate uses that run rather than the
artifact's newest one.

### Rollback

A rollback (`kind: rollback`, `sourcePublicationId`, the same `destinationId` and some of the source's tags) points
those tags back to an earlier publication's image at the same destination, for example when a newer image under
`stable` misbehaves. It moves registry references only; it never rolls back a deployed workload. The source must
still be `published` and the tags must have been published with it. Like a promotion, it waits for a fresh
ValidationRun (trigger `promotion`), so an image that no longer passes current policy is not restored. The `rollback-source` gate re-checks
the source at run time. A rollback may move a **protected** tag back to the image it was published with; ordinary
publications still cannot. **Immutable** tags never move, and such requests are refused. Approval, signing and the
other gates apply as usual, and the result is audited as its own publication.

### Deprecation and retirement

- **Deprecate** (`POST /publications/{id}/deprecation`): marks a published publication as discouraged, with a reason
  of at least 10 characters, or clears the mark. It stays published, promotable and usable.
- **Retire** (`POST /publications/{id}/retire`): ends a publication, with a reason. The state becomes `retired`, so
  it can no longer be promoted or rolled back to. Samuori keeps the record, digest and signatures. It does **not**
  delete tags at the registry, because registries offer no portable tag deletion; remove them there if needed.

Both need the publisher or maintainer role on the project and are audited (`publication.deprecate`,
`publication.undeprecate`, `publication.retire`).

1. **Request** (`POST /publications`, publisher or maintainer on the project). The artifact must be visible to the
   caller, and the destination must be open to the project (SEC-PUB-01). Tags must be valid OCI tags; at most 16 are
   accepted, and the `samuori-` prefix is reserved. Requests are idempotent per caller and key.
2. **Gate check**, re-evaluated when the publication runs, so it does not depend on the request-time check:
   - the destination still exists and is open to the project (`destination`);
   - a promotion-only destination receives only promotions (`kind`);
   - for promotions, the source is still published (`promotion-source`), and the source image's signature there
     verifies with one of the organization's keys (`source-signature`). An unsigned source is never promoted
     (OD-25);
   - for rollbacks, the source is still published at this destination and its tags are not immutable
     (`rollback-source`);
   - when the destination signs or attaches provenance, an active signing key exists (`signing`);
   - when the destination requires it, the approval is approved and still valid (`approval`);
   - the push profile is active (`credentials`);
   - the ValidationRun (the artifact's newest, or the promotion's own) completed with verdict `passed`
     (`validation`);
   - a `restricted` run publishes only to a destination that accepts restricted images (`classification`,
     SEC-OUT-08);
   - the image was not built by a worker revoked as compromised from before the build (`worker-trust`,
     `docs/operators/worker-identity.md`);
   - the stored image has exactly one top-level manifest or index;
   - every tag, checked live at the destination, is new, already points to this image, or is neither protected nor
     covered by immutable tags. A rollback may also move a protected tag back to its source image.
   Any denial makes the publication `blocked`, with the decisions recorded.
3. **Publishing.** Credentials are resolved just in time. The image graph, including referrers such as signatures,
   is copied by digest from the stored OCI archive with oras-go. Then the tags are applied: the immutable build
   reference tag `samuori-<first 12 hex of the image digest>` and the requested tags.
4. **Verifying.** The destination is re-read: the manifest by digest (content verified), every child manifest and
   blob (presence and size), and every tag (SEC-REG-02).
5. **Signing**, when the destination asks for it or a promotion carries signatures. The controller stores a Cosign
   signature and an SLSA provenance attestation, under Cosign's tags and, where the registry supports them, as OCI
   referrers. A promotion also copies the source's signatures and attestations. Everything is read back and
   verified (`signing.md`). A failure fails the publication.
6. **Published.** The destination digest, `repository@digest` reference and signature and attestation references are
   recorded, and the
   `publication.published` audit event is emitted exactly once.

Publications to the same destination are serialized through a lock on the destination, so a tag check and the
tag update never interleave with another Samuori publication (SEC-PUB-02). A writer outside Samuori can still move a tag
between the check and the push; registries offer no compare-and-swap for tags. Interrupted publications are
resumed: pushes by digest are idempotent. A reconciler retries publications that waited for a busy destination.
The images of publications are kept by artifact garbage collection.

## Evidence

Tests run against an in-process OCI registry (go-containerregistry, test-only) and, in the Linux dev VM, against
CNCF Distribution. They cover:

- publish, tags and a single audit event;
- closed destinations at request time and destinations closed before the gate;
- the validation gate;
- protected tags and immutable tags;
- serialization of concurrent publications;
- a destination that loses blobs after the push, so verification fails;
- Basic authentication with good and bad credentials;
- endpoint validation and the loopback guard;
- that credentials never reach audit events or messages;
- that build plans carry no publication data;
- promotion: revalidation decides, a promotion-only destination refuses direct publication, a source signature that
  no longer verifies blocks it, an unsigned source is blocked, and signatures and provenance are carried;
- restricted images to destinations that do not accept them (SEC-OUT-08), and images from compromised workers
  (SEC-WRK-07);
- rollback: tags it may move, a changed destination, a developer, revalidation, protected tags moved back and
  immutable tags refused; deprecation and retirement with reasons, roles, audit events, and no promotion or rollback
  from a retired publication;
- signatures and provenance read back and verified, a missing key, and private keys that stay controller-only;
  in the dev VM, `cosign verify` and `cosign verify-attestation` accept Samuori's artifacts and reject another key.

## Limitations

- ECR and Harbor profiles, `pull` profiles at the mirror, rotation and disablement in the UI: WP27.
- Keys are key-based only; keyless signing, transparency logs and external KMS keys are not supported
  (`signing.md`).

## UI (WP22)

- **Images**: the Image Library per project, with each image's validation and where it is published. An image page
  shows whether it is restricted, lists its publications with their kind, signatures and deprecation, and has
  **Publish image** (destination and tags). Published publications offer **Promote**, **Roll back**,
  **Deprecate** and **Retire**.
- **Approvals**: pending requests with **Decide**, and past decisions.
- **Build supply → Registries & destinations**: registries, push credentials (with **Test**), destinations (projects, tag
  rules, approval, signing, provenance, promotion only, restricted images) and signing keys.
- Project page: switch between manual and automatic publication (maintainers and administrators).
- Image referrers already present in internal storage are copied with the image.
