#!/usr/bin/env bash
# Spike only: re-run railpack planning offline with the mise cache pre-staged read-only. See docs/spikes/railpack.md.
set -uo pipefail
W=/tmp/rp
for app in node python; do
  sudo rm -rf /tmp/rp-stage && sudo cp -a /tmp/railpack /tmp/rp-stage && sudo chown -R 0:0 /tmp/rp-stage
  echo; echo "##### $app: bubblewrap, no network, read-only root and source, disposable writable copy of the staged mise cache"
  sudo bwrap --unshare-net --unshare-pid --unshare-ipc --unshare-uts --unshare-cgroup --die-with-parent --ro-bind / / --tmpfs /tmp --bind /tmp/rp-stage /tmp/railpack \
    --ro-bind "$W/$app" /tmp/src --tmpfs /root --dev /dev --proc /proc \
    railpack plan /tmp/src > "$W/out/$app-plan-sandboxed.json" 2> "$W/out/$app-stderr-sandboxed.txt"
  echo "exit=$?"
  grep -iE 'error|failed|network|dial|lookup' "$W/out/$app-stderr-sandboxed.txt" | head -4
  jq -c '{steps: [.steps[]?.name], deploy: .deploy.startCommand}' "$W/out/$app-plan-sandboxed.json" 2>/dev/null || echo "no plan"
  if diff <(jq -S . "$W/out/$app-plan.json") <(jq -S . "$W/out/$app-plan-sandboxed.json") >/dev/null 2>&1; then echo "plan identical to the networked run"; else
    echo "plan differs from the networked run:"; diff <(jq -S . "$W/out/$app-plan.json") <(jq -S . "$W/out/$app-plan-sandboxed.json") 2>/dev/null | grep '^[<>]' | head -6; fi
done
echo; ls /tmp/rp/out | grep EXECUTED || echo "source-code execution markers: none"
