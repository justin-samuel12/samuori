# Signing and provenance

Status: **Implemented (WP25) for key-based signing with local keys.** Decisions: ADR 0021 (OD-5, OD-25). Publication flow: [`publication.md`](publication.md). Threat rows: TM-61, worker compromise (SEC-WRK-06).

Samuori signs images when it publishes them, so consumers can check that an image came from Samuori and has not changed.
It also records how the image was built in a provenance attestation. Everything happens on the controller; private
keys never reach workers, builds, the UI or the API.

## Keys

- An administrator creates a key (`POST /signing-keys`, Build supply > Registries & destinations > Signing keys). The controller
  generates an ECDSA P-256 key and stores the private key as a **controller-only** local secret
  (`secrets.md`). That secret cannot be bound to a project or read back through the API.
- An organization has at most one **active** key. Creating a key retires the previous one, and retiring a key
  (`POST /signing-keys/{id}/retire`) stops it from signing.
- Retired keys stay listed with their public keys, so existing signatures can still be verified. Promotion accepts
  a source signature from any of the organization's keys.
- Every role can list keys and public keys (`GET /signing-keys`); public keys are not secret.
- Creation and retirement are audited. The portal offers **Retire** on the active key, after a confirmation that
  the key can never sign again and its signatures stay verifiable. Keys are never deleted. `TestSecretAndImageSourceEdits`
  covers the API (administrator-only, a second retire is 404, the retired key keeps its public key).

Signing goes through the `Signer` interface in `internal/signing`, which reports capabilities (key-based, keyless,
attestation types).

### Keys held outside the controller (WP27)

A key can stay in **OpenBao or Vault Transit**, or in **AWS KMS**. Choose the external secret store that connects to
it (`secrets.md`) and give the key reference: `transit/samuori-release` for Transit, or a key ID, ARN or alias for KMS.

- **Registration.** Samuori reads the public key, requires **ECDSA P-256** (`ecdsa-p256` in Transit, `ECC_NIST_P256`
  with `SIGN_VERIFY` in KMS), and asks for a **test signature** that must verify with that public key. A key that
  fails any of these is refused.
- **Transit versions are pinned.** The version registered is the one used. Rotating the Transit key later does not
  change signatures behind Samuori's back; register a new Samuori key to move to the new version.
- **Signing.** Signatures are made by the key service over SHA-256 digests (`prehashed` in Transit, `DIGEST` in KMS)
  and are verified at the destination as before. The private key never leaves the service. The connection's
  credential is a controller-only secret.

## What is signed

A destination chooses **sign images** and/or **attach provenance**. When a publication's push is verified:

1. **Signature.** Cosign's key-based simple-signing payload names the destination repository and the image digest,
   with optional `samuori.publication` and `samuori.signingKey` fields. It is signed with the active key.
2. **Provenance.** An in-toto statement with an SLSA provenance v1 predicate, in a signed DSSE envelope. It records
   the BuildSpec ID, revision and digest, the source snapshot digest, the build method, the plan digest, the
   isolation profile, recipe images by digest, the build run as invocation ID, start and finish times, and the
   executing worker and pool (SEC-WRK-06). It holds no credentials or secret values.
3. Both are stored at the destination (below), read back and verified with the active key before the publication
   is `published`. Their references are recorded on the publication and shown on the image page.

If no key is active, the `signing` gate blocks the publication; if signing or verification fails, the publication
fails. A published image is never left unsigned at a destination that requires signatures.

## Storage (OD-25)

Developers do not need to know how signatures are stored. They see whether an image is signed and how to verify it.

- **Cosign tags, always.** Signatures go under `sha256-<hex>.sig` and attestations under `sha256-<hex>.att`, which
  is what `cosign verify --key` reads. A new signature is added to the layers already there, never replacing them.
- **OCI 1.1 referrers, when available.** After pushing the image, Samuori probes the registry's referrers API
  (`GET /v2/<name>/referrers/<digest>` answering with an image index). If the registry serves it, each signature
  and attestation is also pushed as its own manifest. The manifest has the image as its `subject` and the layer's
  media type as its `artifactType`, so `oras discover` and other OCI 1.1 clients find it. Samuori never writes the
  referrers tag schema (`sha256-<hex>` index tags).
- **Read back.** Samuori requires its new signature to verify in the tag and, where used, among the referrers.
- **Recorded.** The publication records `signatureStorage` (`cosign-tag` or `oci-referrers+cosign-tag`) in the
  API, for operators.

## Promotion

- **Source check (`source-signature` gate).** Before a promotion pushes anything, the source image's signature at
  the source destination must verify with one of the organization's keys, active or retired. It must name the source
  repository, or a repository that image was promoted from. An unsigned or unverifiable source is blocked. A
  promotion never makes an unsigned image look trusted.
- **Carried signatures.** The source's signatures and attestations are copied to the new destination with the
  image, next to the new destination's own signature. If the new destination does not sign, the copied signatures
  are still stored and verified there.

## Verifying

Consumers verify with standard Cosign, using the public key from the Signing keys table:

```
cosign verify --key cosign.pub REGISTRY/REPOSITORY@sha256:...
cosign verify-attestation --key cosign.pub --type slsaprovenance1 REGISTRY/REPOSITORY@sha256:...
```

Samuori does not use a transparency log. With Cosign v3, add `--insecure-ignore-tlog=true`; for a plain-HTTP test
registry also add `--allow-http-registry=true --allow-insecure-registry=true`.

## Runbook: rotation and custody

- **Planned rotation:** create a new key. New publications sign with it, and the old key stays available for
  verification. Distribute the new public key to consumers before or when you rotate.
- **Suspected compromise:** create a new key at once, which retires the old one. Then tell consumers to stop
  trusting the old public key, and republish or promote images that must carry a trusted signature. Samuori cannot
  revoke signatures already pushed to registries.
- **Custody:** the private key exists only in the controller's local secret store, AES-256-GCM encrypted with
  `<data dir>/keys/secrets.key` (`secrets.md`). That key sits on the same host, so encryption protects database copies
  and backups, not a compromised controller. Back up the database and `secrets.key` together; losing either loses the
  signing key, and you then create a new one.

## Evidence

> History: the tests and make targets named below ran at earlier commits and were removed (ADR 0039); restored
> focused tests are marked in `docs/developers/security-test-catalog.md`.

- `internal/signing`: signature and attestation round trips, a different key rejected, a tampered payload rejected,
  an attestation type the signer does not support refused, provenance contents, and combined tags, referrer
  manifests and digest coverage (`TestStorage`).
- `internal/publication`: signed publications read back and verified, the `signing` gate without a key, private keys
  kept controller-only, provenance with worker and pool, and promotion re-verifying a source signature (a forged
  signature blocks). OD-25: tags only on a registry without referrers, and earlier signatures kept on
  republication (`TestSignedPublication`). Referrers plus tags, with no referrers tag schema, on a registry that
  serves the API (`TestReferrerStorage`). Signatures and provenance carried by promotion, and an unsigned source
  blocked (`TestPromotion`).
- `make test-buildkit` in the Linux dev VM (`deploy/dev/lima/provision-signing.sh`, Cosign v3.1.3
  checksum-pinned): `cosign verify` and `cosign verify-attestation` accept Samuori's artifacts on CNCF Distribution and
  reject another key. After a promotion to a second repository, `cosign verify` reports both the carried and the new
  signature, and the carried provenance verifies. With a key in **OpenBao 2.7.1 Transit**, a published image and its
  provenance pass `cosign verify` and `cosign verify-attestation` with the public key Samuori recorded. Transit signing
  stays pinned after the key is rotated, and an RSA Transit key is refused.
- `TestReferrersOnZot` (dev VM, `deploy/dev/lima/provision-zot.sh`): zot 2.1.22, which serves the referrers API,
  receives a signed, attested image. Samuori records `oci-referrers+cosign-tag`, the registry lists the Cosign signature
  and the DSSE provenance attestation as referrers of the image, and the Cosign CLI verifies both.
- After OD-25, the all-in-one acceptance journey passes on a freshly reset clean VM (2026-10-20). It covers a signed
  publication with provenance read back from CNCF Distribution.

## Limitations

- Key-based only. Keyless signing and transparency logs are not supported yet.
- **AWS KMS signing is not verified end to end.** The emulator in the dev VM (moto 5.2.3) does not implement
  `MessageType=DIGEST`, so the test shows only that the request carries Samuori's digest and that registration correctly
  refuses the emulator's signature. Verifying KMS signatures needs a real KMS key, which is pending with the other
  account-dependent checks.
- The referrer manifests use Samuori's artifact types. Cosign's own OCI 1.1 mode has not been tested against them;
  `cosign verify` is tested against the tags.
- **Live referrers evidence is pending.** CNCF Distribution in the dev VM does not serve the referrers API, so Samuori
  stores tags only there (`cosign-tag`, recorded by the test). The referrers path is tested only against
  go-containerregistry's in-process registry. A test against a registry that serves the API live is still needed.
- Signatures are made at publication; images in internal storage are not signed.
