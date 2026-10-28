# 0021. Image signing and vulnerability scanner providers

Status: Accepted, **tentative** for the signing default (2026-10-19, owner decisions OD-5 and OD-10)

## Context
D15 requires scanning, SBOM, signing and provenance. Keyless Sigstore signing records identities and digests in a
public transparency log, which may be unacceptable for private images. Trivy and Grype vulnerability-database terms
have not been verified (`docs/dependencies.md`).

## Decision

### Signing
- Default mode: **key-based signing** with Cosign, using keys that administrators manage through a secrets or KMS
  provider (local encrypted, OpenBao Transit, AWS KMS where available). The tentative approval is revisited at M8
  with spike evidence.
- Signing sits behind a `Signer` interface with capability flags (key-based, keyless, attestation types), so
  keyless Sigstore or other tooling can be added later without changing callers.
- Signatures and attestations are stored against the image digest and verified again during publication and
  promotion. Where and when they are written is decided in "Signature timing and storage (OD-25)" below.

### Scanning
- Scanners sit behind a `Scanner` interface. Each result records scanner name and version, plus vulnerability-DB
  source, version and timestamp.
- **No default scanner is selected or bundled** until the vulnerability database licensing and redistribution
  terms are verified from primary sources and recorded in `docs/dependencies.md`. This covers air-gapped mirroring
  and caching of the DB.
- Until then, development and spikes may use scanners only as operator-provided external tools.

### Implementation (WP25, 2026-10-19)
- `internal/signing`: a `Signer` interface with capability flags; the first signer is a P-256 key generated on the
  controller and kept as a controller-only local secret. OpenBao Transit and AWS KMS signers are WP27.
- Signatures use Cosign's key-based simple-signing format and provenance is an in-toto SLSA v1 statement in a DSSE
  envelope.
- Signatures are made **at publication, per destination**, not in internal storage before publication: the
  simple-signing payload names the destination repository, which is only known then.
- Scanners: Grype and Trivy exec adapters behind `VulnerabilityScanner`; none is a default (OD-10).
- Design and runbook: `docs/architecture/signing.md`.

### Signature timing and storage (OD-25, approved with conditions 2026-10-20)
- Signing happens **only on the controller, at publication**, once the destination repository and image digest are
  known. Each signature names both.
- Every signature and attestation is written under **Cosign's legacy tags** (`sha256-<hex>.sig`, `.att`), which
  `cosign verify --key` reads. When the destination registry serves the **OCI 1.1 referrers API** (probed at
  publication), each one is also pushed as a referrer manifest whose `subject` is the image. Samuori never writes the
  referrers tag schema. The publication records which storage was used (`signatureStorage`).
- New signatures are added to those already stored, never replacing them.
- After pushing, Samuori reads the tag and, where used, the referrers back and requires its own signature to verify in
  each. Otherwise the publication fails.
- Promotion requires a signature on the source image, at the source destination, that verifies with one of the
  organization's keys and names the source repository or one it was promoted from. An unsigned or unverifiable
  source is blocked (`source-signature` gate), never trusted silently.
- Promotion carries the source's signatures and attestations to the new destination, alongside the new
  destination's own.
- Private keys stay out of workers, logs and API responses. Signers sit behind `Signer`, so external key providers
  (WP27) plug in without changing storage.
- Storage is an implementation detail: developers see "signed" and the verification command, not the format.

### External signing keys (WP27, 2026-10-20)
- `signing.Remote` signs through OpenBao or Vault Transit or AWS KMS, using the organization's external secret
  store connections. Keys must be ECDSA P-256 and pass a test signature at registration; Transit key versions are
  pinned. Private keys stay in the key service.

## Consequences
Key rotation and custody runbooks are required before M8 is complete. OD-10 stays open as a task: "verify DB terms,
then select a default".
