#!/bin/bash
# Buildah isolation spike (docs/spikes/buildah-isolation.md): runs the BuildKit isolation probe with rootless
# Buildah as the dedicated build user under uid-scoped strict egress. Dev VM only.
set -euo pipefail
BUILD_USER=samuori-build
BUILD_UID=$(id -u "$BUILD_USER")
HOST_IP=$(ip -4 -o addr show eth0 | awk '{print $4}' | cut -d/ -f1)
SRC=$(cd "$(dirname "$0")/../buildkit-isolation/probe" && pwd)
OUT=${OUT:-/tmp/samuori-buildah-spike}
SECRET="fictitious-spike-secret-$(date +%s)"

sudo rm -rf "$OUT" && mkdir -p "$OUT/log"
W=$(sudo -u "$BUILD_USER" mktemp -d /tmp/samuori-buildah.XXXXXX)
sudo cp -r "$SRC" "$W/ctx" && sudo chown -R "$BUILD_USER" "$W/ctx"
printf '%s' "$SECRET" | sudo -u "$BUILD_USER" tee "$W/secret" >/dev/null
sudo -u "$BUILD_USER" chmod 600 "$W/secret"
sudo -u "$BUILD_USER" tee "$W/registries.conf" >/dev/null <<CONF
unqualified-search-registries = ["docker.io"]
[[registry]]
prefix = "docker.io"
location = "registry-1.docker.io"
[[registry.mirror]]
location = "${HOST_IP}:5000"
insecure = true
CONF
sudo -u "$BUILD_USER" tee "$W/containers.conf" >/dev/null <<CONF
[network]
default_rootless_network_cmd = "slirp4netns"
[engine]
runtime = "runc"
CONF

as_build() {
  sudo -n -u "$BUILD_USER" /usr/bin/env -i HOME="/home/$BUILD_USER" XDG_RUNTIME_DIR="/run/user/$BUILD_UID" PATH=/usr/sbin:/usr/bin:/sbin:/bin \
    CONTAINERS_CONF="$W/containers.conf" CONTAINERS_REGISTRIES_CONF="$W/registries.conf" "$@"
}

egress_on() {
  sudo nft -f - <<NFT
table inet samuori_buildah_spike {
  chain output {
    type filter hook output priority 0; policy accept;
    meta skuid ${BUILD_UID} ct original ip daddr ${HOST_IP} ct original proto-dst { 5000, 8888 } accept
    meta skuid ${BUILD_UID} counter drop
  }
}
NFT
}
egress_off() { sudo nft delete table inet samuori_buildah_spike 2>/dev/null || true; }
trap 'egress_off; sudo rm -rf "$W"' EXIT

buildah_build() {
  local name=$1 network=$2
  as_build buildah --root "$W/root" --runroot "$W/run" --storage-driver overlay build \
    --isolation oci --network "$network" --layers=false --pull=always \
    --secret id=probe_secret,src="$W/secret" --build-arg HOST_IP="$HOST_IP" \
    -f "$W/ctx/Dockerfile" -t "oci-archive:$W/$name.tar" "$W/ctx" >"$OUT/log/$name.log" 2>&1 || echo "build $name exited $?" >>"$OUT/log/$name.log"
}

as_build mkdir -p "/run/user/$BUILD_UID/libpod/tmp"
as_build buildah version >"$OUT/log/version.log" 2>&1
egress_on
buildah_build strict private
egress_off
buildah_build none none

# Limits: the whole build runs in a transient unit, as the rootless-host provider's cgroup limits do for BuildKit.
sudo cp -r "$(dirname "$SRC")/limits" "$W/limits" && sudo chown -R "$BUILD_USER" "$W/limits"
sudo systemd-run --quiet --wait --pipe --collect --uid="$BUILD_USER" -p MemoryMax=1G -p MemorySwapMax=0 -p TasksMax=512 \
  --setenv=HOME="/home/$BUILD_USER" --setenv=XDG_RUNTIME_DIR="/run/user/$BUILD_UID" \
  --setenv=CONTAINERS_CONF="$W/containers.conf" --setenv=CONTAINERS_REGISTRIES_CONF="$W/registries.conf" \
  /usr/bin/buildah --root "$W/root" --runroot "$W/run" --storage-driver overlay build --isolation oci --network none \
  --layers=false -f "$W/limits/Dockerfile" -t "oci-archive:$W/limits.tar" "$W/limits" >"$OUT/log/limits.log" 2>&1 || echo "build limits exited $?" >>"$OUT/log/limits.log"
sudo grep -v "== pids" "$W/limits/Dockerfile" | sudo -u "$BUILD_USER" tee "$W/limits/Dockerfile.memory" >/dev/null
sudo systemd-run --quiet --wait --pipe --collect --uid="$BUILD_USER" -p MemoryMax=1G -p MemorySwapMax=0 -p TasksMax=512 \
  --setenv=HOME="/home/$BUILD_USER" --setenv=XDG_RUNTIME_DIR="/run/user/$BUILD_UID" \
  --setenv=CONTAINERS_CONF="$W/containers.conf" --setenv=CONTAINERS_REGISTRIES_CONF="$W/registries.conf" \
  /usr/bin/buildah --root "$W/root" --runroot "$W/run" --storage-driver overlay build --isolation oci --network none \
  --layers=false -f "$W/limits/Dockerfile.memory" -t "oci-archive:$W/memory.tar" "$W/limits" >"$OUT/log/memory.log" 2>&1 || echo "build memory exited $?" >>"$OUT/log/memory.log"

{
  for name in strict none; do
    echo "== $name: image archive"
    if sudo test -s "$W/$name.tar"; then
      sudo tar -tf "$W/$name.tar" | wc -l | sed 's/^/entries /'
      if sudo grep -a -q "$SECRET" "$W/$name.tar"; then echo "secret FOUND in the archive"; else echo "secret absent from the archive"; fi
    else
      echo "no archive (the build did not finish)"
    fi
  done
  echo "== secret value in Buildah storage"
  if sudo grep -r -a -l "$SECRET" "$W/root" "$W/run" 2>/dev/null; then echo "FOUND"; else echo "absent"; fi
  echo "== limits build"
  grep -E "started|ALLOCATED|stopped|exited|Killed|error" "$OUT/log/limits.log" | tail -5
  echo "== memory build"
  grep -E "ALLOCATED|stopped|exited|Killed|signal|error" "$OUT/log/memory.log" | tail -5
  echo "== build processes left running as $BUILD_USER"
  pgrep -u "$BUILD_USER" -a -f "buildah|slirp4netns|runc" || echo "none"
} >"$OUT/log/checks.log" 2>&1
sudo cp "$W"/*.tar "$OUT/" 2>/dev/null || true
echo "logs in $OUT/log"
