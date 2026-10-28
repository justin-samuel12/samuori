#!/usr/bin/env bash
# Spike only: provisions a disposable VM. See docs/spikes/buildkit-isolation.md.
set -euo pipefail

BUILDKIT_VERSION="${BUILDKIT_VERSION:-v0.34.0}"
ARCH="$(dpkg --print-architecture)"
WORK=/opt/spike
BUILD_USER=bkuser

sudo apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
  uidmap slirp4netns nftables tinyproxy jq curl bzip2 ca-certificates docker.io >/dev/null

sudo mkdir -p "$WORK/dl" "$WORK/bin"
cd "$WORK/dl"

fetch() {
  sudo curl -fsSL --retry 3 -o "$2" "$1"
}

latest_tag() {
  curl -fsSL "https://api.github.com/repos/$1/releases/latest" | jq -r .tag_name
}

fetch "https://github.com/moby/buildkit/releases/download/${BUILDKIT_VERSION}/buildkit-${BUILDKIT_VERSION}.linux-${ARCH}.tar.gz" buildkit.tgz
sudo tar -xzf buildkit.tgz -C "$WORK"

ROOTLESSKIT_VERSION="$(latest_tag rootless-containers/rootlesskit)"
fetch "https://github.com/rootless-containers/rootlesskit/releases/download/${ROOTLESSKIT_VERSION}/rootlesskit-$(uname -m).tar.gz" rootlesskit.tgz
fetch "https://github.com/rootless-containers/rootlesskit/releases/download/${ROOTLESSKIT_VERSION}/SHA256SUMS" rootlesskit.SHA256SUMS
grep "rootlesskit-$(uname -m).tar.gz" rootlesskit.SHA256SUMS | sed "s|rootlesskit-$(uname -m).tar.gz|rootlesskit.tgz|" | sha256sum -c -
sudo tar -xzf rootlesskit.tgz -C "$WORK/bin"

GVISOR_VERSION="$(latest_tag google/gvisor)"
GVISOR_ASSET="gvisor-$(uname -m).tar.bz2"
fetch "https://github.com/google/gvisor/releases/download/${GVISOR_VERSION}/${GVISOR_ASSET}" gvisor.tbz
fetch "https://github.com/google/gvisor/releases/download/${GVISOR_VERSION}/SHA256SUMS" gvisor.SHA256SUMS
grep " ${GVISOR_ASSET}\$" gvisor.SHA256SUMS | sed "s| ${GVISOR_ASSET}\$| gvisor.tbz|" | sha256sum -c -
sudo mkdir -p gvisor && sudo tar -xjf gvisor.tbz -C gvisor
sudo install -m 0755 gvisor/runsc "$WORK/bin/runsc"
sudo cp -r gvisor/gvisor-bin /usr/local/bin/

sudo install -m 0755 "$WORK"/bin/* /usr/local/bin/ 2>/dev/null || true
sudo install -m 0755 "$WORK"/bin/buildkitd "$WORK"/bin/buildctl /usr/local/bin/ 2>/dev/null || true

id "$BUILD_USER" >/dev/null 2>&1 || sudo useradd -m -s /bin/bash "$BUILD_USER"
sudo loginctl enable-linger "$BUILD_USER"

{
  echo "buildkit ${BUILDKIT_VERSION}"
  echo "rootlesskit ${ROOTLESSKIT_VERSION}"
  echo "runsc $(/usr/local/bin/runsc --version | head -1)"
  echo "kernel $(uname -r)"
  echo "os $(. /etc/os-release && echo "$PRETTY_NAME")"
  sha256sum "$WORK/dl/buildkit.tgz" "$WORK/dl/rootlesskit.tgz" "$WORK/dl/gvisor.tbz"
} | sudo tee "$WORK/versions.txt"
