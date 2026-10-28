#!/usr/bin/env bash
# Spike only. Runs every OD-15 scenario; see docs/spikes/buildkit-secrets.md.
set -uo pipefail

U=bkuser
UIDN="$(id -u "$U")"
RT="/run/user/${UIDN}"
SOCK="unix://${RT}/buildkit/buildkitd.sock"
STATE="/home/${U}/.local/share/buildkit"
W="/home/${U}/od15"
SEC="${W}/secrets"
OUT=/tmp/od15/out

as_user() { sudo -u "$U" XDG_RUNTIME_DIR="$RT" HOME="/home/${U}" "$@"; }
say() { printf '\n##### %s\n' "$*"; }

prepare() {
  sudo rm -rf "$W" "$OUT" && sudo mkdir -p "$SEC" "$OUT" && sudo cp -r /tmp/od15/scenarios "$W/"
  for id in alpha alpha2 beta delta zeta; do
    printf 'OD15%s%s' "${id^^}" "$(openssl rand -hex 12)" | sudo tee "$SEC/$id" >/dev/null
  done
  { head -c 2048 /dev/urandom; printf 'OD15GAMMA%s' "$(openssl rand -hex 12)"; head -c 2048 /dev/urandom; } | sudo tee "$SEC/gamma" >/dev/null
  sudo chown -R "$U:" "$W" && sudo chmod 600 "$SEC"/* && sudo chmod 777 "$OUT"
}

marker() { sudo grep -aoE 'OD15[A-Z0-9]+[0-9a-f]{24}' "$SEC/$1"; }

start_daemon() {
  stop_daemon
  as_user mkdir -p "${RT}/buildkit"
  as_user nohup rootlesskit --net=slirp4netns --copy-up=/etc --disable-host-loopback \
    buildkitd --addr "$SOCK" >"$OUT/buildkitd.log" 2>&1 &
  for _ in $(seq 30); do as_user buildctl --addr "$SOCK" debug workers >/dev/null 2>&1 && return; sleep 1; done
  echo "buildkitd failed to start"; tail "$OUT/buildkitd.log"; exit 1
}

stop_daemon() {
  sudo pkill -x buildkitd 2>/dev/null; sudo pkill -x rootlesskit 2>/dev/null; sleep 2
}

probe() {
  local tag="$1" file="$2"; shift 2
  as_user secretprobe -addr "$SOCK" -context "$W/scenarios" -file "$file" -tag "$tag" "$@" 2>&1 | tee "$OUT/$tag.log"
}

all_secrets() { for id in alpha beta gamma delta zeta; do printf -- '-secret %s=%s ' "$id" "$SEC/$id"; done; }

scan_disk() {
  local label="$1"
  for id in alpha alpha2 beta gamma delta zeta; do
    local m; m="$(marker "$id")"
    local hits
    hits="$(sudo grep -rlaF "$m" "$STATE" "$RT" /tmp /var/tmp "/home/${U}" 2>/dev/null \
      | grep -vE "^${SEC}/|^${OUT}/|^/tmp/od15/" | sed "s|^${STATE}|<state>|" | sort -u | head -5)"
    printf '%s disk %-6s %s\n' "$label" "$id" "${hits:-none}"
  done
}

scan_oci() {
  local label="$1" tarball="$2"
  local dir="$OUT/$label-oci"
  rm -rf "$dir" && mkdir -p "$dir" && tar -xf "$tarball" -C "$dir"
  for id in alpha beta gamma delta zeta; do
    local m n=0; m="$(marker "$id")"
    for b in "$dir"/blobs/sha256/*; do
      if zcat -f "$b" 2>/dev/null | grep -qaF "$m"; then n=$((n+1)); fi
    done
    printf '%s oci  %-6s blobs-containing=%d\n' "$label" "$id" "$n"
  done
  for b in "$dir"/blobs/sha256/*; do
    zcat -f "$b" 2>/dev/null | tar -t 2>/dev/null | grep -E '^(certificates|run/secrets|run)(/|$)' | sed "s|^|$label layer-entry |"
  done | sort -u
}

scan_dir() {
  local label="$1" dir="$2"
  for id in alpha beta gamma delta zeta; do
    local m n; m="$(marker "$id")"
    n="$(grep -rlaF "$m" "$dir" 2>/dev/null | wc -l)"
    printf '%s %s %-6s files-containing=%d\n' "$label" "$(basename "$dir")" "$id" "$n"
  done
}

scan_logs() {
  local label="$1"
  for id in alpha alpha2 beta gamma delta zeta; do
    local m; m="$(marker "$id")"
    printf '%s logs %-6s %s\n' "$label" "$id" "$(grep -laF "$m" "$OUT"/*.log 2>/dev/null | xargs -r -n1 basename | tr '\n' ' ' || true)"
  done
}

scan_memory() {
  local label="$1" pid
  pid="$(pgrep -x buildkitd | head -1)"
  [ -z "$pid" ] && { echo "$label memory buildkitd not running"; return; }
  sudo gcore -o "$OUT/core" "$pid" >/dev/null 2>&1
  for id in alpha alpha2 beta gamma delta zeta; do
    local m; m="$(marker "$id")"
    printf '%s memory %-6s occurrences=%s\n' "$label" "$id" "$(sudo grep -aoF "$m" "$OUT/core.$pid" | wc -l)"
  done
  sudo rm -f "$OUT/core.$pid"
}

secret_mounts() {
  local n
  n="$(sudo grep -lE ' /run/secrets/| /certificates/' /proc/[0-9]*/mountinfo 2>/dev/null | wc -l)"
  printf '%s processes-with-secret-mounts=%s sleep60-processes=%s\n' "$1" "$n" "$(pgrep -fc 'sleep 60' || true)"
}

expect_hash() { sudo sha256sum "$SEC/$1" | cut -c1-12; }

prepare
start_daemon
echo "expected digests: alpha=$(expect_hash alpha) alpha2=$(expect_hash alpha2) beta=$(expect_hash beta) gamma=$(expect_hash gamma)"

say "S1 timing, optional and unused secrets, mount targets, outputs (no cache)"
probe s1 timing.Dockerfile $(all_secrets) -no-cache -oci "$OUT/s1.tar" -cache-export "$OUT/s1-cache" -provenance
scan_oci s1 "$OUT/s1.tar"; scan_dir s1 "$OUT/s1-cache"; scan_disk s1; scan_memory s1-after-build

say "S2 cached rebuild"
probe s2 timing.Dockerfile $(all_secrets)

say "S3 revocation after first release (no cache)"
probe s3 timing.Dockerfile $(all_secrets) -no-cache -deny-after alpha=1

say "S4 cancellation by client 3s after release"
probe s4 cancel.Dockerfile -secret alpha="$SEC/alpha" -no-cache -cancel-after-first-secret 3s
secret_mounts s4-immediately; sleep 10; secret_mounts s4-after-10s; scan_disk s4

say "S5 client disconnect (SIGKILL) 3s after release"
as_user secretprobe -addr "$SOCK" -context "$W/scenarios" -file cancel.Dockerfile -tag s5 -secret alpha="$SEC/alpha" -no-cache >"$OUT/s5.log" 2>&1 &
for _ in $(seq 60); do grep -q released "$OUT/s5.log" && break; sleep 0.5; done
sleep 3; sudo pkill -9 -x secretprobe; cat "$OUT/s5.log"
secret_mounts s5-immediately; sleep 10; secret_mounts s5-after-10s; scan_disk s5

say "S6 failing step"
probe s6 fail.Dockerfile -secret alpha="$SEC/alpha" -secret gamma="$SEC/gamma" -no-cache
scan_disk s6

say "S7 concurrent builds with different values for the same id (no cache)"
probe s7a conc.Dockerfile -secret alpha="$SEC/alpha" -no-cache -local "$OUT/s7a" >/dev/null &
s7a=$!
probe s7b conc.Dockerfile -secret alpha="$SEC/alpha2" -no-cache -local "$OUT/s7b" >/dev/null &
s7b=$!
wait "$s7a" "$s7b"
echo "s7a got $(cat "$OUT/s7a/alpha.hash") expected $(expect_hash alpha); s7b got $(cat "$OUT/s7b/alpha.hash") expected $(expect_hash alpha2)"
grep -h GetSecret "$OUT/s7a.log" "$OUT/s7b.log"

say "S8 cache reuse across sessions with different secret values"
probe s8a conc.Dockerfile -secret alpha="$SEC/alpha" -local "$OUT/s8a" >/dev/null
probe s8b conc.Dockerfile -secret alpha="$SEC/alpha2" -local "$OUT/s8b" | grep -E 'CACHED|GetSecret'
echo "s8b result $(cat "$OUT/s8b/alpha.hash"); alpha=$(expect_hash alpha) alpha2=$(expect_hash alpha2)"

say "S9 env= secret mount"
probe s9 env.Dockerfile -secret alpha="$SEC/alpha" -no-cache -oci "$OUT/s9.tar" | grep -E 'log \||GetSecret'
scan_oci s9 "$OUT/s9.tar"

say "S10 build script copies a secret into the image"
probe s10 leak.Dockerfile -secret alpha="$SEC/alpha" -no-cache -oci "$OUT/s10.tar" >/dev/null
scan_oci s10 "$OUT/s10.tar"

say "Final scans"
scan_logs final
scan_memory final-daemon-running
stop_daemon
scan_disk final-after-daemon-stop
