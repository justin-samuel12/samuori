#!/bin/sh
# Prepares the samuori-kvm VM (or any Linux KVM host) for the vm-per-job provider tests: Cloud Hypervisor, the guest
# image build tools and access to /dev/kvm. Run inside the VM; provision-egress.sh adds the docker.io mirror.
set -eu
case "$(uname -m)" in
  aarch64) ch=cloud-hypervisor-static-aarch64 chsum=f192b510eea1c710cbc439d716bb0573c223fc463dbe3e6523788a2b7ef62850 ;;
  x86_64) ch=cloud-hypervisor-static chsum=448af3d4e59b22c2987f7df94c213ad40fb53a10d437e42b5ee6c4fce7c29ecc ;;
  *) echo "unsupported architecture $(uname -m)"; exit 1 ;;
esac
[ -e /dev/kvm ] || { echo "no /dev/kvm: enable (nested) virtualization for this host"; exit 1; }
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq mmdebstrap e2fsprogs zstd >/dev/null
dl=$(mktemp -d)
cd "$dl"
# Digests are the GitHub release asset digests for v53.0.
curl -fsSL -o cloud-hypervisor https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v53.0/$ch
echo "$chsum  cloud-hypervisor" | sha256sum -c -
sudo install -m 0755 cloud-hypervisor /usr/local/bin/cloud-hypervisor
sudo usermod -aG kvm "$(id -un)"
cloud-hypervisor --version
