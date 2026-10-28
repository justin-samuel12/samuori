#!/bin/sh
# Installs pinned rootless BuildKit into the samuori-dev VM for engine tests. Run inside the VM.
set -eu
# BuildKit sums: computed locally, no upstream checksum published (they match the GitHub release asset digests).
case "$(uname -m)" in
  aarch64) arch=arm64 rkarch=aarch64 bksum=0619ca3e3205a32ea2beb115e0242136e4a0716cc1917ba0ceaa4ef4d52382c9 rksum=7a159e79ef3e65e1f2702ab89537bb79de67b433428328b9626cf75786dbe43a ;;
  x86_64) arch=amd64 rkarch=x86_64 bksum=14813b1fb2a40082114e6d386c6fc5e7dd5298176ce7ee1a18bc33e73e109ac3 rksum=0e757c8f80510d764df8c9a5f169805580b48d02329f0a200900e261ecec27ed ;;
  *) echo "unsupported architecture $(uname -m)"; exit 1 ;;
esac
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq uidmap slirp4netns >/dev/null
dl=$(mktemp -d)
cd "$dl"
curl -fsSL -o buildkit.tgz https://github.com/moby/buildkit/releases/download/v0.34.0/buildkit-v0.34.0.linux-$arch.tar.gz
echo "$bksum  buildkit.tgz" | sha256sum -c -
curl -fsSL -o rootlesskit.tgz https://github.com/rootless-containers/rootlesskit/releases/download/v3.3.0/rootlesskit-$rkarch.tar.gz
echo "$rksum  rootlesskit.tgz" | sha256sum -c -
sudo tar -xzf buildkit.tgz -C /usr/local
sudo tar -xzf rootlesskit.tgz -C /usr/local/bin
sudo tee /etc/apparmor.d/usr.local.bin.rootlesskit >/dev/null <<'PROFILE'
abi <abi/4.0>,
include <tunables/global>
profile rootlesskit /usr/local/bin/rootlesskit flags=(unconfined) {
  userns,
}
PROFILE
sudo apparmor_parser -r /etc/apparmor.d/usr.local.bin.rootlesskit
buildctl --version
rootlesskit --version
