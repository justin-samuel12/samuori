# Spike: OCI Image Copy with Signatures and Referrers

Date: 2026-10-19. Status: **Complete; consistent with `registry-and-artifacts.md` (copy referrers and re-verify).
It names the tooling the publisher must use.** Code: `tools/spikes/oci-copy/`. Raw output is kept in the
maintainer's private records.

## Setup
- Two CNCF Distribution `registry:3` instances in a disposable VM.
- Source image: `busybox:1.37`, a 17-platform index.
- Cosign v3.1.3 with a synthetic key pair and a signing config that has no transparency log or timestamp services.
  It produced a signature and an SPDX attestation.
- ORAS attached one extra artifact.
- Copied with `crane copy` (go-containerregistry v0.22.1), `skopeo copy --all` (1.13.3), `oras cp -r` (1.3.5) and
  `cosign copy`. Each copy was verified at the destination with `cosign verify` and `cosign verify-attestation`
  against the public key.

## Results

| Copy method | Digest preserved | Referrers at destination | Signature verifies | Attestation verifies |
|---|---|---|---|---|
| `crane copy` | yes | 0 of 3 | **no** | **no** |
| `skopeo copy --all` | yes | 0 of 3 | **no** | **no** |
| `oras cp -r` | yes | 3 of 3 | yes | yes |
| `cosign copy` (v3) | yes | 0 of 3 (copied 18 legacy tags instead) | **no** | **no** |

Other observations:
- Cosign v3 stores signatures and attestations as **OCI 1.1 referrers** (Sigstore bundle v0.3), not as legacy
  `sha256-…` tags.
- `--tlog-upload=false` is rejected in v3. Key-only signing needs a signing config without a transparency log
  (`cosign signing-config create`).
- `cosign copy` in v3.1.3 did not carry its own v3 referrer bundles.

## Conclusions for M6 and M8
- Digest identity survives every tool tested. **Provenance does not**, unless referrers are copied explicitly.
- The controller's publisher uses a referrer-aware copy: oras-go (Apache-2.0), already a candidate. It then verifies
  the destination digest, signatures and attestations before marking a publication `published` (SEC-REG-02).
  crane- or skopeo-style copies alone are insufficient.
- The destination must support the OCI 1.1 referrers API or the referrers tag fallback. This is a capability check
  per registry: ECR, Harbor and others are verified in their M9 spikes.
- Key-based signing (ADR 0021) uses a v3 signing config without transparency-log services. The adapter must pin
  Cosign v3 behavior; v2 flags do not carry over.
