#!/usr/bin/env bash
# Spike only: digest, referrer and signature behavior when copying images. See docs/spikes/oci-copy.md.
set -uo pipefail
W=/tmp/ocicopy; rm -rf "$W"; mkdir -p "$W"; cd "$W"
SRC=localhost:5001; DST=localhost:5002
for p in 5001 5002; do sudo docker rm -f "reg$p" >/dev/null 2>&1; sudo docker run -d --name "reg$p" -p "127.0.0.1:$p:5000" docker.io/library/registry:3 >/dev/null; done
sleep 2

crane copy docker.io/library/busybox:1.37 "$SRC/app:1.0" --insecure >/dev/null 2>&1
DIGEST="$(crane digest "$SRC/app:1.0" --insecure)"
echo "source index digest: $DIGEST ($(crane manifest "$SRC/app@$DIGEST" --insecure | jq -r '.manifests | length') platforms)"

COSIGN_PASSWORD= cosign generate-key-pair >/dev/null 2>&1
cosign signing-config create --out sc.json >/dev/null 2>&1 && echo "signing config without transparency log or timestamp services: $(jq -c . sc.json)"
CO=(--key cosign.key --signing-config sc.json --allow-insecure-registry --allow-http-registry --yes)
COSIGN_PASSWORD= cosign sign "${CO[@]}" "$SRC/app@$DIGEST" >/dev/null 2>"$W/sign.err" && echo "signed" || { echo "sign failed:"; tail -3 "$W/sign.err"; }
echo '{"spdxVersion":"SPDX-2.3","name":"synthetic-sbom"}' > sbom.json
COSIGN_PASSWORD= cosign attest "${CO[@]}" --type spdxjson --predicate sbom.json "$SRC/app@$DIGEST" >/dev/null 2>"$W/attest.err" && echo "attested" || { echo "attest failed:"; tail -3 "$W/attest.err"; }
echo "synthetic attachment" > note.txt
oras attach --plain-http --artifact-type application/vnd.samuori.spike.note "$SRC/app@$DIGEST" note.txt >/dev/null && echo "oras artifact attached"

echo; echo "source referrers:"; oras discover --plain-http --format tree "$SRC/app@$DIGEST" 2>&1 | tail -n +2 | sed 's/^/  /'
echo "source signature tags (cosign tag scheme):"; crane ls "$SRC/app" --insecure | grep -E '^sha256-' | sed 's/^/  /' || echo "  none"

verify() {
  local ref="$1"
  local s a
  cosign verify --key cosign.pub --insecure-ignore-tlog --allow-insecure-registry --allow-http-registry "$ref" >/dev/null 2>&1 && s=ok || s=FAIL
  cosign verify-attestation --key cosign.pub --type spdxjson --insecure-ignore-tlog --allow-insecure-registry --allow-http-registry "$ref" >/dev/null 2>&1 && a=ok || a=FAIL
  echo "signature=$s attestation=$a"
}
echo; echo "source: $(verify "$SRC/app@$DIGEST")"

report() {
  local name="$1" repo="$DST/$1"
  local d n
  d="$(crane digest "$repo:1.0" --insecure 2>/dev/null || echo missing)"
  n="$(oras discover --plain-http --format json "$repo@$d" 2>/dev/null | jq '[.referrers // .manifests // [] | .[]] | length' 2>/dev/null || echo 0)"
  echo "$name: digest-preserved=$([ "$d" = "$DIGEST" ] && echo yes || echo "no ($d)") referrers=$n sig-tags=$(crane ls "$repo" --insecure 2>/dev/null | grep -cE '^sha256-') $(verify "$repo@$d")"
}

echo; echo "##### copies"
crane copy "$SRC/app:1.0" "$DST/crane:1.0" --insecure >/dev/null 2>&1; report crane
skopeo copy --all --src-tls-verify=false --dest-tls-verify=false "docker://$SRC/app:1.0" "docker://$DST/skopeo:1.0" >/dev/null 2>&1; report skopeo
oras cp -r --from-plain-http --to-plain-http "$SRC/app:1.0" "$DST/oras:1.0" >/dev/null 2>&1; report oras
cosign copy --allow-insecure-registry --allow-http-registry --force "$SRC/app@$DIGEST" "$DST/cosign:1.0" >/dev/null 2>"$W/ccopy.err"; report cosign
