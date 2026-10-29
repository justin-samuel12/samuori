# Release Process

Status: **local dry runs only.** Nothing is published yet. Every build is a development build
(`<VERSION>-dev+<commit>`, where the tracked `VERSION` file holds the next release number, such as `0.1.4`) until
the first release. Raise `VERSION` when a build should sort above the ones already installed; `VERSION=` on the
make command line still overrides it.

## Build and sign

1. Start from a clean `main` with `make ci` passing.
2. Run `make vuln` and resolve any reachable finding.
3. Create the release key once with `make release-key`. It writes `dist/signing/release` (the private Ed25519 key,
   never copied to hosts; back it up, because a lost key means reinstalling every host with a new signer file),
   `release.pub` and `allowed_signers`, never overwrites them, and no other target deletes `dist/signing`. Then build
   the signed artifacts:

   ```
   make release-artifacts RELEASE_ARCH=arm64   # and RELEASE_ARCH=amd64; RELEASE_KEY=<path> for another key
   ```

   `dist/artifacts/` then holds the release tarball (`samuori-server`, `samuori-worker`, `samuori`, the UI, systemd units,
   `install.sh` and the Compose files), the SPDX SBOM, `THIRD_PARTY_NOTICES.txt`, `SHA256SUMS` and
   `SHA256SUMS.sig`. `make release` also writes `THIRD_PARTY_NOTICES.txt` into the UI's `licenses/` folder, so every
   installation shows it under **About Samuori → View third-party notices**.
4. Verify against the allowed signers file (default `dist/signing/allowed_signers`), one line per release key, for
   example `release@samuori.invalid namespaces="samuori-release" ssh-ed25519 AAAA…`:

   ```
   make release-verify
   ```

   It checks every checksum and the signature over `SHA256SUMS`. A changed artifact or checksum file fails.

A dry run on 2026-10-20 built and verified the arm64 artifacts with a disposable key, and both kinds of
tampering failed verification.

## Update bundle

`make update` builds every architecture in `UPDATE_ARCHES` (default `amd64 arm64`) and writes one signed file,
`dist/samuori-<version>.samuoriupdate`, for upload under **Organization > Upgrades & diagnostics** (ADR 0045,
`../operators/upgrades.md#self-update`). It is an uncompressed tar of `manifest.json` (format, version, worker
protocol, architectures), `samuori-<version>-linux-<arch>.tar.gz` per architecture, `SHA256SUMS` over the manifest and
the archives, and `SHA256SUMS.sig` (the same `samuori-release` SSH signature as above, made by
`samuori-server update-bundle`). Hosts verify their own archive against the same signed `SHA256SUMS`. Each archive also holds `CHANGES.tsv`, the
`git log --no-merges` history the Upgrades page shows; it is in the archive rather than the manifest because
controllers before 0.1.4 refuse unknown manifest fields and bundle entries.

A dry run on 2026-10-22 bundled two placeholder archives with a disposable key; `ssh-keygen -Y verify` and
`shasum -c` accepted the extracted files and the host helper's checksum lookup found the archive. A full
`make update` (UI and both architectures) was not run.

## Before the first publication

- Samuori's own `LICENSE` and `NOTICE`, which wait for the outbound license decision.
- A published allowed-signers file and key custody. The key must never be in the repository or on a build worker.
- A license review of every component in `THIRD_PARTY_NOTICES.txt`, and of the base image of the optional
  `controller-pgtools` image (`docs/dependencies.md`).
- Restoring the test suites (ADR 0039), or the owner's explicit acceptance of releasing without them.
- A hosted CI that runs `make ci` and the release steps from a clean checkout.
