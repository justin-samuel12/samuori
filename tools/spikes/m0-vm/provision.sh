#!/usr/bin/env bash
# Spike only: provisions the disposable VM for M0 experiments 6-9. Pinned versions; checksums verified where upstream publishes them.
set -euo pipefail
ARCH="$(dpkg --print-architecture)"
OUT=/opt/m0
sudo mkdir -p "$OUT/dl" && cd "$OUT/dl"
sudo apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq uidmap slirp4netns bubblewrap docker.io skopeo jq gdb curl >/dev/null

fetch() { sudo curl -fsSL --retry 3 -o "$2" "$1"; }
verify() { echo "$1  $2" | sha256sum -c -; }
gh_sum() { curl -fsSL "$1" | grep -E " \*?$2\$" | awk '{print $1}'; }

fetch "https://github.com/moby/buildkit/releases/download/v0.34.0/buildkit-v0.34.0.linux-${ARCH}.tar.gz" buildkit.tgz
verify 0619ca3e3205a32ea2beb115e0242136e4a0716cc1917ba0ceaa4ef4d52382c9 buildkit.tgz
fetch "https://github.com/rootless-containers/rootlesskit/releases/download/v3.3.0/rootlesskit-$(uname -m).tar.gz" rootlesskit.tgz
verify 7a159e79ef3e65e1f2702ab89537bb79de67b433428328b9626cf75786dbe43a rootlesskit.tgz
sudo tar -xzf buildkit.tgz -C /usr/local && sudo tar -xzf rootlesskit.tgz -C /usr/local/bin

RP=v0.42.0
fetch "https://github.com/railwayapp/railpack/releases/download/${RP}/railpack-${RP}-arm64-unknown-linux-musl.tar.gz" railpack.tgz || \
  fetch "https://github.com/railwayapp/railpack/releases/download/${RP}/railpack-${RP}-arm64-unknown-linux-gnu.tar.gz" railpack.tgz
sudo tar -xzf railpack.tgz -C /usr/local/bin railpack 2>/dev/null || sudo tar -xzf railpack.tgz -C /usr/local/bin

CO=v3.1.3
fetch "https://github.com/sigstore/cosign/releases/download/${CO}/cosign-linux-${ARCH}" cosign
verify "$(gh_sum "https://github.com/sigstore/cosign/releases/download/${CO}/cosign_checksums.txt" "cosign-linux-${ARCH}")" cosign
sudo install -m 0755 cosign /usr/local/bin/cosign

ORAS="$(curl -fsSL https://api.github.com/repos/oras-project/oras/releases/latest | jq -r .tag_name)"
fetch "https://github.com/oras-project/oras/releases/download/${ORAS}/oras_${ORAS#v}_linux_${ARCH}.tar.gz" oras.tgz
verify "$(gh_sum "https://github.com/oras-project/oras/releases/download/${ORAS}/oras_${ORAS#v}_checksums.txt" "oras_${ORAS#v}_linux_${ARCH}.tar.gz")" oras.tgz
sudo tar -xzf oras.tgz -C /usr/local/bin oras

GGCR=v0.22.1
fetch "https://github.com/google/go-containerregistry/releases/download/${GGCR}/go-containerregistry_Linux_arm64.tar.gz" ggcr.tgz
verify "$(gh_sum "https://github.com/google/go-containerregistry/releases/download/${GGCR}/checksums.txt" "go-containerregistry_Linux_arm64.tar.gz")" ggcr.tgz
sudo tar -xzf ggcr.tgz -C /usr/local/bin crane

sudo tee /etc/apparmor.d/usr.local.bin.rootlesskit >/dev/null <<'P'
abi <abi/4.0>,
include <tunables/global>
profile rootlesskit /usr/local/bin/rootlesskit flags=(unconfined) {
  userns,
}
P
sudo apparmor_parser -r /etc/apparmor.d/usr.local.bin.rootlesskit 2>/dev/null || true
id bkuser >/dev/null 2>&1 || sudo useradd -m -s /bin/bash bkuser
sudo loginctl enable-linger bkuser
sudo docker pull -q docker.io/library/registry:3 >/dev/null
sudo docker image inspect --format '{{index .RepoDigests 0}}' docker.io/library/registry:3

{ buildkitd --version; rootlesskit --version; railpack --version 2>&1 | head -1; cosign version 2>/dev/null | grep -E '^GitVersion'; oras version | head -1; crane version; skopeo --version; bwrap --version; uname -r; . /etc/os-release; echo "$PRETTY_NAME"; sha256sum "$OUT"/dl/*; } | sudo tee "$OUT/versions.txt"
