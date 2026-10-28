#!/bin/sh
# Installs the zot registry (minimal build, OCI distribution-spec 1.1 with the referrers API) in the samuori-dev VM for
# the referrers publication test, which starts its own throwaway registry. Development use only.
set -eu
case "$(uname -m)" in
  aarch64) arch=arm64 sum=4224c23122c43638f92875d6fb4f23564472d2584a348cb057440bcf4b43bf08 ;;
  x86_64) arch=amd64 sum=a053169f97b4ceaa051e054d53971799188a398cb36acbaf5e08622ec8664c44 ;;
  *) echo "unsupported architecture $(uname -m)"; exit 1 ;;
esac
dl=$(mktemp -d)
cd "$dl"
curl -fsSL -o zot https://github.com/project-zot/zot/releases/download/v2.1.22/zot-linux-$arch-minimal
echo "$sum  zot" | sha256sum -c -
sudo install -m 0755 zot /usr/local/bin/zot
cd / && rm -rf "$dl"
zot --version 2>&1 | head -1 || true
