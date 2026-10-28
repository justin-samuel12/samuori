#!/bin/sh
# Installs pinned k3s, Helm and BuildKit (to build the Samuori image) into the samuori-k3s VM. Run inside the VM.
set -eu
[ "$(uname -m)" = aarch64 ] || { echo "the k3s checksum below is for arm64"; exit 1; }
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq openssl jq >/dev/null
dl=$(mktemp -d)
cd "$dl"
curl -fsSL -o k3s "https://github.com/k3s-io/k3s/releases/download/v1.37.1%2Bk3s1/k3s-arm64"
echo "a1561ca4aef8b99f5588a840d4468ac39c8c1cd92470bbcb9634823fa221741a  k3s" | sha256sum -c -
sudo install -m 0755 k3s /usr/local/bin/k3s
sudo ln -sf k3s /usr/local/bin/kubectl
sudo tee /etc/systemd/system/k3s.service >/dev/null <<'UNIT'
[Unit]
Description=k3s for the Samuori HA rehearsal
After=network-online.target
[Service]
ExecStart=/usr/local/bin/k3s server --disable traefik --disable servicelb --disable metrics-server --write-kubeconfig-mode 0644
Restart=always
Delegate=yes
KillMode=process
LimitNOFILE=1048576
[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl daemon-reload
sudo systemctl enable --now k3s
until kubectl get nodes 2>/dev/null | grep -q ' Ready'; do sleep 2; done
curl -fsSL -o buildkit.tgz https://github.com/moby/buildkit/releases/download/v0.34.0/buildkit-v0.34.0.linux-arm64.tar.gz
echo "0619ca3e3205a32ea2beb115e0242136e4a0716cc1917ba0ceaa4ef4d52382c9  buildkit.tgz" | sha256sum -c -
sudo tar -xzf buildkit.tgz -C /usr/local
kubectl get nodes
