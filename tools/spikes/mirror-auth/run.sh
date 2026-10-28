#!/usr/bin/env bash
# Spike only (OD-14): does a token-authenticated pull-through mirror enforce per-project access to cached content?
set -uo pipefail
W=/tmp/od14; sudo rm -rf "$W"; mkdir -p "$W/certs" "$W/auth" "$W/dc"; cd "$W"
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq apache2-utils >/dev/null
PUSH_PW="$(openssl rand -hex 12)"; MIRROR_PW="$(openssl rand -hex 12)"; P1_PW="$(openssl rand -hex 12)"; P2_PW="$(openssl rand -hex 12)"
htpasswd -Bbn pusher "$PUSH_PW" > auth/htpasswd; htpasswd -Bbn mirror "$MIRROR_PW" >> auth/htpasswd

pkill -f tokensrv 2>/dev/null; nohup /tmp/tokensrv -addr 0.0.0.0:5099 -cert "$W/certs/token.crt" -passwords "p1:$P1_PW,p2:$P2_PW" > "$W/tokensrv.log" 2>&1 &
sleep 1
sudo docker rm -f od14-upstream od14-mirror >/dev/null 2>&1; sudo docker network rm od14 >/dev/null 2>&1; sudo docker network create od14 >/dev/null
cat > upstream.yml <<Y
version: 0.1
log: { level: warn }
storage: { filesystem: { rootdirectory: /var/lib/registry } }
http: { addr: ":5000" }
auth: { htpasswd: { realm: upstream, path: /auth/htpasswd } }
Y
cat > mirror.yml <<Y
version: 0.1
log: { level: warn }
storage: { filesystem: { rootdirectory: /var/lib/registry } }
http: { addr: ":5000" }
auth:
  token: { realm: "http://127.0.0.1:5099/token", service: samuori-mirror, issuer: samuori-spike, rootcertbundle: /certs/token.crt }
proxy: { remoteurl: "http://od14-upstream:5000", username: mirror, password: "$MIRROR_PW" }
Y
sudo docker run -d --name od14-upstream --network od14 -p 127.0.0.1:5010:5000 -v "$W/upstream.yml:/etc/distribution/config.yml:ro" -v "$W/auth:/auth:ro" docker.io/library/registry:3 >/dev/null
sudo docker run -d --name od14-mirror --network od14 -p 127.0.0.1:5011:5000 -v "$W/mirror.yml:/etc/distribution/config.yml:ro" -v "$W/certs:/certs:ro" docker.io/library/registry:3 >/dev/null
sleep 2

export DOCKER_CONFIG="$W/dc"
crane auth login localhost:5010 -u pusher -p "$PUSH_PW" >/dev/null 2>&1
crane copy --platform linux/arm64 docker.io/library/busybox:1.37 localhost:5010/private/app:1.0 --insecure >/dev/null 2>&1
crane copy --platform linux/arm64 docker.io/library/busybox:1.37 localhost:5010/private/other:1.0 --insecure >/dev/null 2>&1
echo "project-1 confidential layer $(openssl rand -hex 8)" > unique.txt && tar -cf unique.tar unique.txt
crane append -f unique.tar -b localhost:5010/private/app:1.0 -t localhost:5010/private/app:1.0 --insecure >/dev/null 2>&1
UNIQUE="$(crane manifest localhost:5010/private/app:1.0 --insecure | jq -r '.layers[-1].digest')"
echo "seeded upstream: private/app (with a unique layer $UNIQUE) and private/other"

ACCEPT='Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'
token() { curl -s -u "$1:$2" "http://127.0.0.1:5099/token?service=samuori-mirror&scope=repository:$3:pull" | jq -r .token; }
code() { curl -s -o /dev/null -w '%{http_code}' ${2:+-H "Authorization: Bearer $2"} -H "$ACCEPT" "http://127.0.0.1:5011$1"; }

T1="$(token p1 "$P1_PW" private/app)"; T2APP="$(token p2 "$P2_PW" private/app)"; T2OTHER="$(token p2 "$P2_PW" private/other)"
echo
echo "T1 p1 manifest private/app (first pull, fills cache):        $(code /v2/private/app/manifests/1.0 "$T1")   expect 200"
echo "T1 p1 unique layer via private/app:                          $(code /v2/private/app/blobs/$UNIQUE "$T1")   expect 200"
echo "T2 p2 manifest private/app (cached, not authorized):         $(code /v2/private/app/manifests/1.0 "$T2APP")   expect 401"
echo "T2 p2 unique layer via private/app (cached):                 $(code /v2/private/app/blobs/$UNIQUE "$T2APP")   expect 401"
echo "T3 anonymous manifest private/app:                           $(code /v2/private/app/manifests/1.0)   expect 401"
echo "T4 p2 manifest private/other (authorized):                   $(code /v2/private/other/manifests/1.0 "$T2OTHER")   expect 200"
echo "T5 p2 project-1 unique layer through private/other:          $(code /v2/private/other/blobs/$UNIQUE "$T2OTHER")   expect 404 (anything 2xx = leak)"
sudo docker stop od14-upstream >/dev/null
echo "-- upstream stopped: responses now come from the mirror's cache only"
echo "T6 p1 cached manifest private/app:                           $(code /v2/private/app/manifests/1.0 "$T1")   expect 200"
echo "T6 p2 cached manifest private/app:                           $(code /v2/private/app/manifests/1.0 "$T2APP")   expect 401"
echo "T6 p2 unique layer through private/other (cache only):       $(code /v2/private/other/blobs/$UNIQUE "$T2OTHER")   expect 404"
echo
echo "token decisions:"; sed -E 's/^[0-9\/: ]+//' "$W/tokensrv.log" | sort | uniq -c
