#!/bin/sh
# Installs pinned Helm and kubeconform for chart checks (tools/helm/check.sh). Run inside the VM.
set -eu
case "$(uname -m)" in
aarch64) arch=arm64 helm_sum=31c5794dd55c66a51e6b7d2e2ac7a114ae8b1de41ff1d9ba51748ac973b06a08 kc_sum=1f53fc8e81258197a35e8603054162a5af1de8c5af13746c71ab680d9534ed87 ;;
x86_64) arch=amd64 helm_sum=86584a54def73570558f66f5111cc53dfed56689637ae32c1201205d494f54fb kc_sum=9bc2bffbf71f261128533edaf912153948b7ff238f9a531ae6d34466ec287883 ;;
*) echo "unsupported architecture"; exit 1 ;;
esac
dl=$(mktemp -d)
cd "$dl"
curl -fsSL -o helm.tgz "https://get.helm.sh/helm-v4.3.0-linux-$arch.tar.gz"
echo "$helm_sum  helm.tgz" | sha256sum -c -
curl -fsSL -o kubeconform.tgz "https://github.com/yannh/kubeconform/releases/download/v0.8.0/kubeconform-linux-$arch.tar.gz"
echo "$kc_sum  kubeconform.tgz" | sha256sum -c -
tar -xzf helm.tgz
sudo install -m 0755 "linux-$arch/helm" /usr/local/bin/helm
tar -xzf kubeconform.tgz kubeconform
sudo install -m 0755 kubeconform /usr/local/bin/kubeconform
helm version --short
kubeconform -v
