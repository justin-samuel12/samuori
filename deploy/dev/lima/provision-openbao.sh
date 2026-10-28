#!/bin/sh
# Installs a pinned OpenBao into the samuori-dev VM for the secret-provider and KMS-signer tests, which start their own
# throwaway dev servers. The checksum comes from the release's checksums.txt, verified with its Sigstore bundle.
set -eu
case "$(uname -m)" in
  aarch64) arch=arm64 sum=2b3807d90f224df05d4fe1fdede7f052d2227596c648a56bd291381f8fd01840 ;;
  x86_64) arch=amd64 sum=0e2f1ce10d124e03112b50dd2fbec6b78003783253bc3a91587938f39d1e2243 ;;
  *) echo "unsupported architecture $(uname -m)"; exit 1 ;;
esac
dl=$(mktemp -d)
cd "$dl"
curl -fsSL -o bao.tgz https://github.com/openbao/openbao/releases/download/v2.7.1/openbao_2.7.1_linux_$arch.tar.gz
echo "$sum  bao.tgz" | sha256sum -c -
tar -xzf bao.tgz bao
sudo install -m 0755 bao /usr/local/bin/bao
bao version
