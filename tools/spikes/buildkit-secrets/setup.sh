#!/usr/bin/env bash
# Spike only. See docs/spikes/buildkit-secrets.md.
set -euo pipefail

BUILDKIT_VERSION=v0.34.0
ROOTLESSKIT_VERSION=v3.3.0
ARCH="$(dpkg --print-architecture)"

sudo apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq uidmap slirp4netns gdb jq >/dev/null

cd /tmp
curl -fsSL -o buildkit.tgz "https://github.com/moby/buildkit/releases/download/${BUILDKIT_VERSION}/buildkit-${BUILDKIT_VERSION}.linux-${ARCH}.tar.gz"
echo "0619ca3e3205a32ea2beb115e0242136e4a0716cc1917ba0ceaa4ef4d52382c9  buildkit.tgz" | sha256sum -c -
curl -fsSL -o rootlesskit.tgz "https://github.com/rootless-containers/rootlesskit/releases/download/${ROOTLESSKIT_VERSION}/rootlesskit-$(uname -m).tar.gz"
echo "7a159e79ef3e65e1f2702ab89537bb79de67b433428328b9626cf75786dbe43a  rootlesskit.tgz" | sha256sum -c -
sudo tar -xzf buildkit.tgz -C /usr/local
sudo tar -xzf rootlesskit.tgz -C /usr/local/bin

sudo tee /etc/apparmor.d/usr.local.bin.rootlesskit >/dev/null <<'PROFILE'
abi <abi/4.0>,
include <tunables/global>
profile rootlesskit /usr/local/bin/rootlesskit flags=(unconfined) {
  userns,
}
PROFILE
sudo apparmor_parser -r /etc/apparmor.d/usr.local.bin.rootlesskit 2>/dev/null || true

id bkuser >/dev/null 2>&1 || sudo useradd -m -s /bin/bash bkuser
sudo loginctl enable-linger bkuser
sudo install -m 0755 /tmp/od15/secretprobe /usr/local/bin/secretprobe
{ buildkitd --version; rootlesskit --version; uname -r; . /etc/os-release; echo "$PRETTY_NAME"; } | tee /tmp/od15/versions.txt
