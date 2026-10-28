#!/bin/sh
# Installs pinned Grype and its database into the samuori-dev VM to test the scanner adapter (docs/architecture/validation.md).
# Grype's database terms are unverified (OD-10): development use only, never bundled.
set -eu
case "$(uname -m)" in
  aarch64) arch=arm64 sum=29f47391dc283aa79fcc38e65224cd61f64dec0ecfd0db7074128ebf8ff23514 ;;
  x86_64) arch=amd64 sum=0a9ee97ef5ae2ee953b0a80098105052e846cdbe319a57d808b519c33cd1343d ;;
  *) echo "unsupported architecture $(uname -m)"; exit 1 ;;
esac
dl=$(mktemp -d)
cd "$dl"
curl -fsSL -o grype.tgz https://github.com/anchore/grype/releases/download/v0.120.1/grype_0.120.1_linux_$arch.tar.gz
echo "$sum  grype.tgz" | sha256sum -c -
tar -xzf grype.tgz grype
sudo install -m 0755 grype /usr/local/bin/grype
grype version | head -2
GRYPE_CHECK_FOR_APP_UPDATE=false grype db update
