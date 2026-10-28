#!/bin/sh
# Installs pinned Syft into the samuori-dev VM for SBOM checks (docs/architecture/validation.md). Run inside the VM.
set -eu
case "$(uname -m)" in
  aarch64) arch=arm64 sum=dfdf0537610113edbefe1f1fc6548bc957b2d77439636ec824fcf0e10d46d054 ;;
  x86_64) arch=amd64 sum=c069905b391cc4c20a5ba65ad5c10be2a7ba074f8ea6ad203e24d14e303dad47 ;;
  *) echo "unsupported architecture $(uname -m)"; exit 1 ;;
esac
dl=$(mktemp -d)
cd "$dl"
curl -fsSL -o syft.tgz https://github.com/anchore/syft/releases/download/v1.54.1/syft_1.54.1_linux_$arch.tar.gz
echo "$sum  syft.tgz" | sha256sum -c -
tar -xzf syft.tgz syft
sudo install -m 0755 syft /usr/local/bin/syft
syft version | head -2
