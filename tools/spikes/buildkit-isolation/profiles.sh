#!/usr/bin/env bash
# Spike only. Usage: profiles.sh <command> [args]. See docs/spikes/buildkit-isolation.md.
set -euo pipefail

BUILD_USER=bkuser
BUILD_UID="$(id -u "$BUILD_USER")"
RUNTIME_DIR="/run/user/${BUILD_UID}"
SOCK="unix://${RUNTIME_DIR}/buildkit/buildkitd.sock"
HOST_IP="$(ip -4 -o addr show eth0 | awk '{print $4}' | cut -d/ -f1)"
PROBE_DIR="/home/${BUILD_USER}/probe"
LOG=/tmp/spike-logs
mkdir -p "$LOG"

as_build_user() {
  sudo -u "$BUILD_USER" XDG_RUNTIME_DIR="$RUNTIME_DIR" HOME="/home/${BUILD_USER}" "$@"
}

apparmor_rootlesskit() {
  sudo tee /etc/apparmor.d/usr.local.bin.rootlesskit >/dev/null <<'PROFILE'
abi <abi/4.0>,
include <tunables/global>
profile rootlesskit /usr/local/bin/rootlesskit flags=(unconfined) {
  userns,
}
PROFILE
  sudo apparmor_parser -r /etc/apparmor.d/usr.local.bin.rootlesskit
}

fixtures() {
  sudo rm -rf "$PROBE_DIR" && sudo cp -r /tmp/buildkit-isolation/probe "$PROBE_DIR"
  echo "spike-secret-value-7f3a" | sudo tee "$PROBE_DIR/../probe_secret" >/dev/null
  sudo chown -R "$BUILD_USER:" "$PROBE_DIR" "$PROBE_DIR/../probe_secret"
  pgrep -f "http.server 18080" >/dev/null || (nohup python3 -m http.server 18080 --bind 0.0.0.0 >/dev/null 2>&1 &)
  sudo tee /etc/tinyproxy/tinyproxy.conf >/dev/null <<CONF
User tinyproxy
Group tinyproxy
Port 8888
Listen ${HOST_IP}
Timeout 30
LogFile "/var/log/tinyproxy/tinyproxy.log"
LogLevel Info
Allow 127.0.0.1
Allow ${HOST_IP%.*}.0/24
ConnectPort 443
FilterDefaultDeny Yes
FilterType ere
Filter "/etc/tinyproxy/filter"
CONF
  printf '%s\n' '^registry\.npmjs\.org$' '^registry-1\.docker\.io$' '^auth\.docker\.io$' \
    '^production\.cloudflare\.docker\.com$' '^[a-z0-9-]+\.r2\.cloudflarestorage\.com$' | sudo tee /etc/tinyproxy/filter >/dev/null
  echo "file r /etc/tinyproxy/filter," | sudo tee /etc/apparmor.d/local/tinyproxy >/dev/null
  sudo apparmor_parser -r /etc/apparmor.d/tinyproxy
  sudo systemctl restart tinyproxy
}

mirror() {
  sudo docker rm -f samuori-mirror >/dev/null 2>&1 || true
  sudo docker run -d --name samuori-mirror -p "${HOST_IP}:5000:5000" \
    -e REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io docker.io/library/registry:3 >/dev/null
  as_build_user mkdir -p "/home/${BUILD_USER}/.config/buildkit"
  as_build_user tee "/home/${BUILD_USER}/.config/buildkit/buildkitd.toml" >/dev/null <<TOML
[registry."docker.io"]
  mirrors = ["${HOST_IP}:5000"]
[registry."${HOST_IP}:5000"]
  http = true
TOML
}

start_rootless() {
  local runtime="${1:-runc}" extra=()
  [ "$runtime" = runsc ] && extra=(--oci-worker-binary=/usr/local/bin/runsc-buildkit)
  stop_rootless
  as_build_user mkdir -p "${RUNTIME_DIR}/buildkit"
  as_build_user env HTTPS_PROXY="${EGRESS_PROXY:-}" HTTP_PROXY="${EGRESS_PROXY:-}" NO_PROXY=localhost \
    nohup rootlesskit --net=slirp4netns --copy-up=/etc --disable-host-loopback \
    buildkitd --addr "$SOCK" "${extra[@]}" >"$LOG/buildkitd-${runtime}.log" 2>&1 &
  for _ in $(seq 30); do as_build_user buildctl --addr "$SOCK" debug workers >/dev/null 2>&1 && return 0; sleep 1; done
  tail -20 "$LOG/buildkitd-${runtime}.log"; return 1
}

start_rootless_limited() {
  stop_rootless
  sudo systemctl reset-failed samuori-spike-buildkitd 2>/dev/null || true
  sudo systemd-run --unit=samuori-spike-buildkitd --uid="$BUILD_USER" \
    -p MemoryMax="${MEM_MAX:-1G}" -p MemorySwapMax=0 -p OOMPolicy=continue -p TasksMax="${TASKS_MAX:-256}" -p RuntimeMaxSec=1800 \
    --setenv=XDG_RUNTIME_DIR="$RUNTIME_DIR" --setenv=HOME="/home/${BUILD_USER}" \
    rootlesskit --net=slirp4netns --copy-up=/etc --disable-host-loopback buildkitd --addr "$SOCK" >/dev/null
  for _ in $(seq 30); do as_build_user buildctl --addr "$SOCK" debug workers >/dev/null 2>&1 && return 0; sleep 1; done
  sudo journalctl -u samuori-spike-buildkitd --no-pager | tail -20; return 1
}

probe_limits() {
  as_build_user cp -r /tmp/buildkit-isolation/limits "/home/${BUILD_USER}/limits"
  as_build_user buildctl --addr "$SOCK" build --frontend dockerfile.v0 \
    --local context="/home/${BUILD_USER}/limits" --local dockerfile="/home/${BUILD_USER}/limits" \
    --no-cache --progress=plain 2>&1 | tee "$LOG/limits-$1.log" | grep -E "^#[0-9]+ [0-9.]+ |ERROR|error:" | sed -E 's/^#[0-9]+ [0-9.]+ //'
}

stop_rootless() {
  sudo systemctl stop samuori-spike-buildkitd 2>/dev/null || true
  sudo systemctl reset-failed samuori-spike-buildkitd 2>/dev/null || true
  sudo pkill -u "$BUILD_USER" -f buildkitd 2>/dev/null || true
  sudo pkill -u "$BUILD_USER" -f rootlesskit 2>/dev/null || true
  sleep 1
}

probe() {
  local name="$1"
  as_build_user buildctl --addr "$SOCK" build --frontend dockerfile.v0 \
    --local context="$PROBE_DIR" --local dockerfile="$PROBE_DIR" \
    --opt build-arg:HOST_IP="$HOST_IP" \
    --secret id=probe_secret,src="/home/${BUILD_USER}/probe_secret" \
    --no-cache --progress=plain 2>&1 | tee "$LOG/probe-${name}.log" | grep -E '^#[0-9]+ [0-9.]+ ' | sed -E 's/^#[0-9]+ [0-9.]+ //'
}

egress_on() {
  sudo nft -f - <<NFT
table inet samuori_spike {
  chain output {
    type filter hook output priority 0; policy accept;
    meta skuid ${BUILD_UID} ct original ip daddr ${HOST_IP} ct original proto-dst { 5000, 8888 } accept
    meta skuid ${BUILD_UID} counter drop
  }
}
NFT
}

egress_off() {
  sudo nft delete table inet samuori_spike 2>/dev/null || true
}

"$@"
