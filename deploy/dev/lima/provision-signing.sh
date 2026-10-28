#!/bin/sh
# Installs pinned Cosign into the samuori-dev VM to check that its signatures interoperate (docs/architecture/signing.md).
set -eu
case "$(uname -m)" in
  aarch64) arch=arm64 sum=c5d324e091826b0d7a78eb16fef316450b4eb9aaec045611c08ba06f5e73220a ;;
  x86_64) arch=amd64 sum=4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71 ;;
  *) echo "unsupported architecture $(uname -m)"; exit 1 ;;
esac
dl=$(mktemp -d)
cd "$dl"
curl -fsSL -o cosign https://github.com/sigstore/cosign/releases/download/v3.1.3/cosign-linux-$arch
echo "$sum  cosign" | sha256sum -c -
sudo install -m 0755 cosign /usr/local/bin/cosign
cosign version 2>&1 | grep -i gitversion
