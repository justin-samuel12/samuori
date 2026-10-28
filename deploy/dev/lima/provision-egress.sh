#!/bin/sh
# Prepares the samuori-dev VM for strict-egress tests: a dedicated build user, nftables and a docker.io pull-through
# mirror (docs/architecture/egress.md). Run inside the VM after provision-buildkit.sh.
set -eu
case "$(uname -m)" in
  aarch64) arch=arm64 sum=09d26f88d2c0f161bd1b8bfc6c123571cc3d291dcca8af5477b3cacc4ec95e73 ;;
  x86_64) arch=amd64 sum=40df2224d410f72ae425c3371873b078bbdbda3b8b612be9571f0e6751f3acc8 ;;
  *) echo "unsupported architecture $(uname -m)"; exit 1 ;;
esac
me=$(id -un)
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nftables >/dev/null
getent group samuori >/dev/null || sudo groupadd --system samuori
id samuori-build >/dev/null 2>&1 || sudo useradd --system --create-home --shell /usr/sbin/nologin --gid samuori samuori-build
id samuori-mirror >/dev/null 2>&1 || sudo useradd --system --no-create-home --shell /usr/sbin/nologin samuori-mirror
sudo usermod -aG samuori "$me"
grep -q '^samuori-build:' /etc/subuid || sudo usermod --add-subuids 300000-365535 --add-subgids 300000-365535 samuori-build
sudo loginctl enable-linger samuori-build
sudo tee /etc/sudoers.d/samuori-build >/dev/null <<SUDO
Defaults:$me !use_pty
$me ALL=(samuori-build) NOPASSWD: /usr/local/bin/rootlesskit, /usr/bin/pkill, /usr/bin/env
SUDO
sudo chmod 0440 /etc/sudoers.d/samuori-build
sudo visudo -cq
sudo install -d -o "$me" -g samuori -m 2770 /var/lib/samuori-dev/daemons

dl=$(mktemp -d)
cd "$dl"
curl -fsSL -o registry.tgz https://github.com/distribution/distribution/releases/download/v3.1.2/registry_3.1.2_linux_$arch.tar.gz
echo "$sum  registry.tgz" | sha256sum -c -
tar -xzf registry.tgz registry
sudo install -m 0755 registry /usr/local/bin/registry
sudo install -d -o samuori-mirror -m 0750 /var/lib/samuori-dev/mirror
sudo install -d -m 0755 /etc/samuori-dev
sudo tee /etc/samuori-dev/mirror.yml >/dev/null <<'CFG'
version: 0.1
storage:
  filesystem:
    rootdirectory: /var/lib/samuori-dev/mirror
http:
  addr: :5000
proxy:
  remoteurl: https://registry-1.docker.io
CFG
sudo tee /etc/systemd/system/samuori-dev-mirror.service >/dev/null <<'UNIT'
[Unit]
Description=Samuori development docker.io pull-through mirror
After=network-online.target

[Service]
User=samuori-mirror
ExecStart=/usr/local/bin/registry serve /etc/samuori-dev/mirror.yml
Restart=on-failure

[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl daemon-reload
sudo systemctl enable --now samuori-dev-mirror.service
registry --version
