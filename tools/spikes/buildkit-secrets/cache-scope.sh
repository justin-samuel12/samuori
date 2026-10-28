#!/usr/bin/env bash
# Spike only (OD-19): scoped cache reuse for secret-bearing builds on fresh daemons. See docs/spikes/cache-isolation.md.
set -uo pipefail
U=bkuser; UIDN="$(id -u $U)"; RT="/run/user/$UIDN"; SOCK="unix://$RT/buildkit/buildkitd.sock"
W=/home/$U/od19; SEC=$W/secrets; CACHE=$W/cache; OUT=/tmp/od19
as_user() { sudo -u "$U" XDG_RUNTIME_DIR="$RT" HOME="/home/$U" "$@"; }
sudo rm -rf "$W" "$OUT"; sudo mkdir -p "$SEC" "$CACHE" "$OUT" "$W/ctx"
sudo cp /tmp/od15/scenarios/cache.Dockerfile "$W/ctx/Dockerfile"
for v in p1v1 p1v2 p2v1; do printf 'OD19%s%s' "${v^^}" "$(openssl rand -hex 12)" | sudo tee "$SEC/$v" >/dev/null; done
sudo chown -R "$U:" "$W"; sudo chmod 600 "$SEC"/*; sudo chmod 777 "$OUT"
h() { sudo sha256sum "$SEC/$1" | cut -c1-12; }
echo "expected derived values: p1v1=$(h p1v1) p1v2=$(h p1v2) p2v1=$(h p2v1)"

fresh_build() {
  local label="$1" secret="$2" import="$3" export="$4"
  sudo pkill -x buildkitd; sudo pkill -x rootlesskit; sleep 1
  local state="/home/$U/bk-state-$label"; sudo rm -rf "$state"
  as_user mkdir -p "$RT/buildkit"
  as_user nohup rootlesskit --net=slirp4netns --copy-up=/etc --disable-host-loopback buildkitd --addr "$SOCK" --root "$state" >"$OUT/$label-daemon.log" 2>&1 &
  for _ in $(seq 30); do as_user buildctl --addr "$SOCK" debug workers >/dev/null 2>&1 && break; sleep 1; done
  local args=(-addr "$SOCK" -context "$W/ctx" -file Dockerfile -tag "$label" -secret "alpha=$SEC/$secret" -local "$OUT/$label-fs")
  [ -n "$import" ] && args+=(-cache-import "$CACHE/$import")
  [ -n "$export" ] && args+=(-cache-export "$CACHE/$export")
  as_user secretprobe "${args[@]}" >"$OUT/$label.log" 2>&1
  local cached gets
  cached="$(grep -c 'vertex CACHED.*RUN' "$OUT/$label.log")"
  gets="$(grep -c 'GetSecret' "$OUT/$label.log")"
  echo "$label: secret=$secret import=${import:-none} export=${export:-none} cachedRunSteps=$cached secretRequests=$gets derived=$(sudo cat "$OUT/$label-fs/derived" 2>/dev/null)"
  sudo pkill -x buildkitd; sudo pkill -x rootlesskit; sleep 1; sudo rm -rf "$state"
}

echo; echo "##### scope = (project, binding version set); every build on a fresh daemon with a fresh state dir"
fresh_build c1-first      p1v1 ""            scope-p1-v1
fresh_build c2-same-scope p1v1 scope-p1-v1   scope-p1-v1
fresh_build c3-rotated    p1v2 scope-p1-v2   scope-p1-v2
fresh_build c4-nocache    p1v1 ""            ""
echo; echo "##### negative controls: import deliberately crosses the scope boundary"
fresh_build c5-cross-version p1v2 scope-p1-v1 ""
fresh_build c6-cross-project p2v1 scope-p1-v1 ""

echo; echo "##### secret markers in exported caches"
for v in p1v1 p1v2 p2v1; do m="$(sudo grep -aoE 'OD19[A-Z0-9]+[0-9a-f]{24}' "$SEC/$v")"; echo "$v: files-containing=$(sudo grep -rlaF "$m" "$CACHE" | wc -l)"; done
sudo du -sh "$CACHE"/* | sed "s|$CACHE/||"
