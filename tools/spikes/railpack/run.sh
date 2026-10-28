#!/usr/bin/env bash
# Spike only: does railpack planning execute source code, and does it work sandboxed? See docs/spikes/railpack.md.
set -uo pipefail
W=/tmp/rp; rm -rf "$W"; mkdir -p "$W/node" "$W/python" "$W/out"
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq strace >/dev/null

cat > "$W/node/package.json" <<'J'
{"name":"probe","version":"1.0.0","engines":{"node":">=22"},
 "scripts":{"preinstall":"touch /tmp/rp/out/EXECUTED-preinstall","build":"echo build","start":"node index.js"},
 "dependencies":{}}
J
echo '{"name":"probe","lockfileVersion":3,"requires":true,"packages":{"":{"name":"probe","version":"1.0.0"}}}' > "$W/node/package-lock.json"
echo 'require("http").createServer((q,s)=>s.end("ok")).listen(3000)' > "$W/node/index.js"
printf 'import os\nopen("/tmp/rp/out/EXECUTED-setup-py","w").close()\nfrom setuptools import setup\nsetup(name="probe")\n' > "$W/python/setup.py"
printf 'flask==3.1.0\n' > "$W/python/requirements.txt"
printf 'from flask import Flask\napp = Flask(__name__)\n' > "$W/python/main.py"

railpack --version 2>&1 | head -1

for app in node python; do
  echo; echo "##### $app: unsandboxed, network allowed (strace)"
  rm -rf ~/.cache/railpack ~/.local/share/mise 2>/dev/null
  strace -f -qq -e trace=execve,connect -o "$W/out/$app-strace.txt" railpack plan "$W/$app" > "$W/out/$app-plan.json" 2> "$W/out/$app-stderr.txt"
  echo "exit=$?"
  echo "programs executed:"; grep -oE 'execve\("[^"]+"' "$W/out/$app-strace.txt" | sed 's/execve("//' | sort | uniq -c
  echo "executed from source dir:"; grep -E "execve\(\"$W/$app" "$W/out/$app-strace.txt" | head -3 || true
  echo "remote connections:"; grep -oE 'sin6?_addr=inet_(addr|pton)\([^)]*"[0-9a-f.:]+"' "$W/out/$app-strace.txt" | grep -oE '"[0-9a-f.:]+"' | grep -vE '"(127\.0\.0\.53|::1|127\.0\.0\.1)"' | sort -u | head -8
  echo "plan summary:"; jq -c '{steps: [.steps[]?.name], deploy: .deploy.startCommand, caches: (.caches // {} | keys)}' "$W/out/$app-plan.json" 2>/dev/null || head -c 300 "$W/out/$app-plan.json"
done

echo; echo "##### source-code execution markers"
ls "$W/out" | grep EXECUTED || echo "none (preinstall and setup.py were not executed)"

MISE_CACHE="$(find ~/.cache ~/.local -maxdepth 4 -type f -name mise 2>/dev/null | head -1)"
echo; echo "##### staged mise binary: ${MISE_CACHE:-not found}"

for app in node python; do
  echo; echo "##### $app: sandboxed (bubblewrap: no network, read-only root and source, private /tmp)"
  bwrap --unshare-all --die-with-parent --ro-bind / / --tmpfs /tmp --bind "$HOME/.cache" "$HOME/.cache" \
    --ro-bind "$W/$app" /src --dev /dev --proc /proc \
    railpack plan /src > "$W/out/$app-plan-sandboxed.json" 2> "$W/out/$app-stderr-sandboxed.txt"
  echo "exit=$?"
  tail -3 "$W/out/$app-stderr-sandboxed.txt"
  jq -c '{steps: [.steps[]?.name], deploy: .deploy.startCommand}' "$W/out/$app-plan-sandboxed.json" 2>/dev/null || echo "no plan"
  diff <(jq -S 'del(.. | .version?)' "$W/out/$app-plan.json" 2>/dev/null) <(jq -S 'del(.. | .version?)' "$W/out/$app-plan-sandboxed.json" 2>/dev/null) >/dev/null && echo "plan identical to unsandboxed (ignoring versions)" || echo "plan differs from unsandboxed"
done
