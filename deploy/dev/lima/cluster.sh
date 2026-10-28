#!/bin/sh
# Disposable three-controller cluster on Lima VMs: samuori-clean (PostgreSQL, a TLS zot registry for blobs, controller 1),
# samuori-remote and samuori-ctrl3 (controllers 2 and 3). `test` exercises join with acceptance and removal (ADR 0045).
# See docs/developers/local-dev.md#cluster-vms.
set -eu

usage() {
	cat <<EOF
Usage: $0 <command>
  up      build the release, recreate the three VMs, install and join three controllers
  test    remove samuori-ctrl3 unwiped and check it cannot route back, check the refusals, wipe and rejoin it, then
          remove the leader
  update  roll a signed bundle out to the three controllers (leader last), then a broken one that must roll back
          and pause (needs make release-key before up)
  update-all
          roll a signed bundle out at 100%: one secondary first, which must take the lead, then the rest at once
  down    delete the VMs
EOF
	exit 2
}

repo=$(cd "$(dirname "$0")/../../.." && pwd)
lima=$repo/deploy/dev/lima
vms="samuori-clean samuori-remote samuori-ctrl3"
db_pw=cluster-db-pw-1
admin_pw=Cluster-Owner-Pass-9
first=lima-samuori-clean.internal
api_vm=samuori-clean

exists() { limactl list -q 2>/dev/null | grep -qx "$1"; }
on() { vm=$1; shift; limactl shell "$vm" sh -c "$*"; }
host_of() { echo "lima-$1.internal"; }
step() { echo "== $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

down() {
	for vm in $vms; do
		exists "$vm" && { echo "deleting $vm"; limactl delete -f "$vm" >/dev/null 2>&1; }
	done
	return 0
}

copy_release() {
	COPYFILE_DISABLE=1 tar --no-xattrs -C "$repo/dist/samuori-linux-arm64" -czf - . |
		on "$1" 'rm -rf ~/samuori-release && mkdir -p ~/samuori-release && tar -xzf - -C ~/samuori-release'
	if [ -f "$repo/dist/signing/allowed_signers" ]; then
		on "$1" 'cat > ~/samuori-release/allowed_signers' <"$repo/dist/signing/allowed_signers"
	fi
}

# Hosts trust the local release key when there is one, so updates can be tested.
signers() { [ -f "$repo/dist/signing/allowed_signers" ] && echo "--release-signers ./allowed_signers" || true; }

# api METHOD PATH [JSON]: an administrator call on controller \$api_vm; prints the body, then the status on its own line.
api() {
	on "$api_vm" "
		j=/tmp/samuori-admin.jar; b=https://127.0.0.1:8443/api/v1
		[ -s \$j ] || curl -sk -c \$j -o /dev/null -H 'Content-Type: application/json' \
			-d '{\"username\":\"admin\",\"password\":\"$admin_pw\"}' \$b/auth/login
		t=\$(awk '\$6==\"samuori_csrf\"{print \$7}' \$j)
		curl -sk -b \$j -c \$j -X $1 -H \"X-CSRF-Token: \$t\" -H 'Content-Type: application/json' \
			${3:+-d '$3'} -w '\n%{http_code}\n' \$b$2"
}
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
body() { sed '$d'; }
code() { tail -1; }

infra() {
	step "PostgreSQL and a TLS registry on samuori-clean"
	on samuori-clean "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -q postgresql openssl >/dev/null 2>&1
		c=\$(ls -d /etc/postgresql/*/main)
		echo \"listen_addresses = '*'\" | sudo tee -a \$c/postgresql.conf >/dev/null
		echo 'hostssl samuori samuori 0.0.0.0/0 scram-sha-256' | sudo tee -a \$c/pg_hba.conf >/dev/null
		sudo systemctl restart postgresql
		sudo -u postgres psql -q -c \"CREATE ROLE samuori LOGIN PASSWORD '$db_pw'\" -c 'CREATE DATABASE samuori OWNER samuori'"
	limactl shell samuori-clean sh <"$lima/provision-zot.sh" >/dev/null
	on samuori-clean "set -e; sudo mkdir -p /etc/zot /var/lib/zot; cd /tmp
		openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -subj /CN=dev-ca -keyout ca.key -out ca.crt 2>/dev/null
		openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=$first -keyout zot.key -out zot.csr 2>/dev/null
		printf 'subjectAltName=DNS:$first\n' >san.ext
		openssl x509 -req -in zot.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 2 -extfile san.ext -out zot.crt 2>/dev/null
		sudo cp zot.crt zot.key /etc/zot/
		printf '{\"storage\":{\"rootDirectory\":\"/var/lib/zot\"},\"http\":{\"address\":\"0.0.0.0\",\"port\":\"5000\",\"tls\":{\"cert\":\"/etc/zot/zot.crt\",\"key\":\"/etc/zot/zot.key\"}}}' | sudo tee /etc/zot/config.json >/dev/null
		sudo systemd-run --unit zot /usr/local/bin/zot serve /etc/zot/config.json >/dev/null"
	ca=$(on samuori-clean 'cat /tmp/ca.crt')
	for vm in $vms; do
		printf '%s\n' "$ca" | on "$vm" 'sudo tee /usr/local/share/ca-certificates/samuori-dev-ca.crt >/dev/null && sudo update-ca-certificates >/dev/null 2>&1'
	done
}

join() {
	vm=$1 host=$(host_of "$1")
	step "join $host"
	issued=$(api POST /system/cluster/join-tokens '{"ttlSeconds":900}')
	[ "$(echo "$issued" | code)" = 201 ] || fail "join token: $issued"
	jc=$(echo "$issued" | body | json 'd["code"]')
	on "$vm" "sudo rm -f /tmp/join.log; cd ~/samuori-release && sudo systemd-run --unit samuori-join-test --collect -p WorkingDirectory=\$PWD \
		sh -c './samuori-join controller $jc --host $host >/tmp/join.log 2>&1'" >/dev/null
	shown=
	for _ in $(seq 1 300); do
		shown=$(on "$vm" "sed -n 's/^verification code: //p' /tmp/join.log 2>/dev/null")
		[ -n "$shown" ] && break
		sleep 2
	done
	[ -n "$shown" ] || fail "$host printed no verification code: $(on "$vm" 'tail -5 /tmp/join.log')"
	req=$(api GET /system/cluster/join-requests | body | json "[r['id']+' '+r['verificationCode'] for r in d['items'] if r['host']=='$host' and r['state']=='pending'][0]")
	[ "${req#* }" = "$shown" ] || fail "verification codes differ: host $shown, portal ${req#* }"
	echo "verification code matches: $shown"
	r=$(api POST "/system/cluster/join-requests/${req%% *}/decision" '{"decision":"accept","reason":"codes match"}')
	case $(echo "$r" | code) in 2??) ;; *) fail "accept: $r" ;; esac
	for _ in $(seq 1 120); do
		on "$vm" 'systemctl is-active -q samuori-join-test' || break
		sleep 2
	done
	on "$vm" "grep -q 'is running' /tmp/join.log" || fail "$host join: $(on "$vm" 'tail -8 /tmp/join.log')"
	echo "$host joined"
}

controllers() { api GET /system/upgrades | body | json "' '.join(sorted(c['hostname']+(':leader' if c['leader'] else '')+('' if c['live'] else ':down') for c in d['controllers']))"; }

wait_controllers() {
	for _ in $(seq 1 60); do
		[ "$(controllers | wc -w | tr -d ' ')" = "$1" ] && return 0
		sleep 3
	done
	fail "expected $1 controllers, got: $(controllers)"
}

up() {
	cd "$repo"
	step "building the release"
	make release >/dev/null
	down
	for vm in $vms; do
		yml=$lima/samuori-remote.yml
		[ "$vm" = samuori-clean ] && yml=$lima/samuori-clean.yml
		step "creating $vm"
		limactl start --tty=false --name "$vm" "$yml" >/dev/null 2>&1
		copy_release "$vm"
	done
	infra
	step "init $first"
	on samuori-clean "umask 077; printf '%s' '$admin_pw' | sudo tee /root/admin-password >/dev/null
		cd ~/samuori-release && sudo SAMUORI_DATABASE='postgres://samuori:$db_pw@$first:5432/samuori?sslmode=require' ./install.sh --release . $(signers) \
			--role controller --host $first init --oci-repository $first:5000/samuori --oci-allow-private-network --external-url https://$first:8443 \
			--organization 'Cluster Test' --admin-username admin --admin-password-file /root/admin-password" | tail -3
	for _ in $(seq 1 60); do
		[ "$(api GET /system/upgrades | code)" = 200 ] && break
		sleep 2
	done
	join samuori-remote
	join samuori-ctrl3
	wait_controllers 3
	echo "controllers: $(controllers)"
}

removal_state() { api GET /system/upgrades | body | json "[r['state']+' '+r.get('outcome','') for r in d.get('removals',[]) if r['hostname']=='$1'][-1:]"; }

# remove HOST [OUTCOME]: removes a controller and expects its removal to end with OUTCOME (default wiped).
remove() {
	host=$1 want=${2:-wiped}
	step "remove $host"
	# A controller that joined again counts once NATS takes it back, up to five minutes later.
	for _ in $(seq 1 60); do
		r=$(api POST "/system/cluster/controllers/$host/remove" '{"reason":"cluster test"}')
		case $(echo "$r" | code) in
		202) break ;;
		409) echo "$r" | grep -q "replicas current" || fail "remove $host: $r"; sleep 10 ;;
		*) fail "remove $host: $r" ;;
		esac
	done
	[ "$(echo "$r" | code)" = 202 ] || fail "remove $host was still refused: $r"
	echo "removal accepted"
	for _ in $(seq 1 90); do
		s=$(removal_state "$host")
		case $s in *removed*) break ;; esac
		sleep 3
	done
	echo "removal: $s"
	case $s in *"removed $want"*) ;; *) fail "$host removal ended as: $s" ;; esac
}

expect_refused() {
	r=$(api POST "/system/cluster/controllers/$1/remove" '{"reason":"cluster test"}')
	[ "$(echo "$r" | code)" = 409 ] || fail "removing $1 was not refused: $r"
	echo "refused as expected: $(echo "$r" | body | json 'd["message"]')"
}

wiped() {
	sleep 10
	on "$1" 'test ! -e /etc/samuori && test ! -e /var/lib/samuori && ! systemctl cat samuori-controller.service >/dev/null 2>&1' ||
		fail "$1 still has Samuori files or units"
	echo "$1 wiped"
}

# Every controller must leave unavailable within two minutes; degraded is allowed while a stream has fewer members
# than replicas.
health_all() {
	for vm in "$@"; do
		st=
		for _ in $(seq 1 40); do
			st=$(on "$vm" 'curl -sk https://127.0.0.1:8443/api/v1/system/health' | json 'd["status"]+" "+",".join(c["name"] for c in d["components"] if c["status"]!="ok")')
			case $st in unavailable*) sleep 3 ;; *) break ;; esac
		done
		echo "$vm health: $st"
		case $st in unavailable*) fail "$vm is unavailable" ;; esac
	done
}

test_cluster() {
	cd "$repo"
	echo "controllers: $(controllers)"
	step "remove samuori-ctrl3 without wiping it: its NATS member keeps running with valid certificates (OD-27)"
	on samuori-ctrl3 'sudo systemctl disable --now samuori-wipe.path >/dev/null 2>&1; sudo systemctl stop samuori-controller'
	remove lima-samuori-ctrl3 not-confirmed
	on samuori-ctrl3 'systemctl is-active -q samuori-nats' || fail "samuori-ctrl3's NATS member is not running, so the check proves nothing"
	wait_controllers 2
	echo "controllers: $(controllers)"
	ip3=$(on samuori-remote 'getent hosts lima-samuori-ctrl3.internal' | awk '{print $1}')
	routed=
	for _ in $(seq 1 36); do
		routed=$(for v in samuori-clean samuori-remote; do on "$v" "ss -tn state established '( sport = :6222 or dport = :6222 )'" | grep -cE "$ip3(\]|:)" ; done | paste -sd+ - | bc)
		[ "$routed" = 0 ] && break
		sleep 5
	done
	[ "$routed" = 0 ] || fail "the unwiped samuori-ctrl3 still has $routed NATS routes to the remaining members"
	sleep 30
	routed=$(for v in samuori-clean samuori-remote; do on "$v" "ss -tn state established '( sport = :6222 or dport = :6222 )'" | grep -cE "$ip3(\]|:)" ; done | paste -sd+ - | bc)
	[ "$routed" = 0 ] || fail "the unwiped samuori-ctrl3 routed back: $routed routes"
	echo "the unwiped samuori-ctrl3 cannot route to the remaining members"
	expect_refused lima-samuori-remote
	health_all samuori-clean samuori-remote
	step "wipe samuori-ctrl3 by hand and rejoin it"
	on samuori-ctrl3 'sudo samuori-uninstall --yes >/dev/null'
	wiped samuori-ctrl3
	copy_release samuori-ctrl3
	join samuori-ctrl3
	wait_controllers 3
	leader=$(controllers | tr ' ' '\n' | sed -n 's/:leader$//p')
	vm=${leader#lima-}
	api_vm=$(for v in $vms; do [ "$v" = "$vm" ] || echo "$v"; done | head -1)
	step "remove the leader $leader (through $api_vm)"
	remove "$leader"
	wiped "$vm"
	wait_controllers 2
	echo "controllers: $(controllers)"
	health_all $(for v in $vms; do [ "$v" = "$vm" ] || echo "$v"; done)
	echo "CLUSTER TEST PASSED"
}

# upload FILE: sends a bundle to the controller on $api_vm.
upload() {
	limactl copy "$1" "$api_vm:/tmp/bundle.samuoriupdate"
	on "$api_vm" "
		j=/tmp/samuori-admin.jar; t=\$(awk '\$6==\"samuori_csrf\"{print \$7}' \$j)
		curl -sk -b \$j -H \"X-CSRF-Token: \$t\" -F bundle=@/tmp/bundle.samuoriupdate -w '\n%{http_code}\n' https://127.0.0.1:8443/api/v1/system/releases"
}

# rollout VERSION [PERCENT]: checks, starts and follows a rollout; prints its final line (state, pause reason, steps).
rollout() {
	pct=${2:-25}
	pf=$(api GET "/system/releases/$1/preflight?wavePercent=$pct")
	echo "$pf" | body | json '"\n".join("  check %-22s %-7s %s" % (c["name"], c["state"], c["detail"]) for c in d["checks"])' >&2
	[ "$(echo "$pf" | body | json 'd["ready"]')" = True ] || fail "the checks for $1 did not pass"
	r=$(api POST /system/rollouts "{\"version\":\"$1\",\"reason\":\"cluster update test\",\"wavePercent\":$pct}")
	[ "$(echo "$r" | code)" = 201 ] || fail "start: $r"
	id=$(echo "$r" | body | json 'd["id"]')
	last=
	for _ in $(seq 1 120); do
		sleep 10
		now=$(api GET "/system/rollouts/$id" 2>/dev/null | body | json '" ".join([d["state"], "paused: "+d.get("pauseReason","") if d["paused"] else "", "|", "; ".join("%s %s" % (s["name"], s["state"]) for s in sorted(d["steps"], key=lambda s: s["seq"]))])' 2>/dev/null) || continue
		[ "$now" = "$last" ] || { echo "  $now" >&2; last=$now; }
		case $now in running*paused:*|succeeded*|failed*|cancelled*) break ;; esac
	done
	echo "$id" >"$rollout_file"
	echo "$now"
}

versions() { api GET /system/upgrades | body | json "' '.join(sorted(c['hostname']+'='+c['version'] for c in d['controllers'] if c['live']))"; }

update_cluster() {
	cd "$repo"
	rollout_file=$(mktemp)
	trap 'rm -f "$rollout_file"' EXIT
	[ -f dist/signing/release ] || fail "no release key: run make release-key, then up"
	# SAMUORI_UPDATE_PATCH picks the patch release to move to (default: one above the VERSION file the VMs run); the
	# broken release is the one after it.
	base=$(cat VERSION) hash=$(git rev-parse --short HEAD)
	patch=${SAMUORI_UPDATE_PATCH:-$((${base##*.} + 1))}
	good=${base%.*}.$patch-dev+$hash
	step "update to $good"
	make update VERSION="$good" UPDATE_ARCHES=arm64 >/dev/null
	leader=$(controllers | tr ' ' '\n' | sed -n 's/:leader$//p')
	r=$(upload "dist/samuori-$good.samuoriupdate")
	[ "$(echo "$r" | code)" = 201 ] || fail "upload: $r"
	final=$(rollout "$good")
	case $final in succeeded*) ;; *) fail "the rollout ended: $final" ;; esac
	lastHost=$(echo "$final" | sed 's/.*| //' | tr ';' '\n' | tail -1 | awk '{print $1}')
	[ "$lastHost" = "$leader" ] || fail "the leader $leader was not updated last (last: $lastHost)"
	echo "leader $leader updated last"
	v=$(versions)
	echo "versions: $v"
	for c in $v; do [ "${c#*=}" = "$good" ] || fail "$c is not on $good"; done
	health_all $vms

	broken=${base%.*}.$((patch + 1))-dev+broken
	step "a broken release $broken must roll back"
	work=$(mktemp -d)
	cp -a dist/samuori-linux-arm64 "$work/samuori-linux-arm64"
	printf '#!/bin/sh\necho "deliberately broken release" >&2\nexit 1\n' >"$work/samuori-linux-arm64/samuori-server"
	COPYFILE_DISABLE=1 tar --no-xattrs -C "$work" -czf "$work/samuori-$broken-linux-arm64.tar.gz" samuori-linux-arm64
	go run -ldflags "-X samuori.invalid/samuori/internal/version.Version=$broken" ./cmd/samuori-server update-bundle \
		-key dist/signing/release -out "$work/samuori-$broken.samuoriupdate" "$work/samuori-$broken-linux-arm64.tar.gz" >/dev/null
	r=$(upload "$work/samuori-$broken.samuoriupdate")
	rm -rf "$work"
	[ "$(echo "$r" | code)" = 201 ] || fail "upload: $r"
	final=$(rollout "$broken")
	case $final in running*paused:*) echo "paused as expected: ${final#*paused: }" ;; *) fail "the broken rollout did not pause: $final" ;; esac
	echo "$final" | grep -q "rolled-back\|rolled back\|failed" || fail "no host reports a failure or rollback: $final"
	v=$(versions)
	echo "versions: $v"
	for c in $v; do [ "${c#*=}" = "$good" ] || fail "$c did not go back to $good"; done
	r=$(api POST "/system/rollouts/$(cat "$rollout_file")/cancel" '{}')
	case $(echo "$r" | code) in 2??) echo "rollout stopped" ;; *) fail "stop: $r" ;; esac
	health_all $vms
	echo "CLUSTER UPDATE TEST PASSED"
}

# update_all: a 100% rollout (ADR 0045 as amended 2026-10-25).
update_all() {
	cd "$repo"
	rollout_file=$(mktemp)
	trap 'rm -f "$rollout_file"' EXIT
	[ -f dist/signing/release ] || fail "no release key: run make release-key, then up"
	base=$(cat VERSION)
	good=${base%.*}.$((${base##*.} + 1))-dev+all
	step "update everything to $good at 100%"
	make update VERSION="$good" UPDATE_ARCHES=arm64 >/dev/null
	leader=$(controllers | tr ' ' '\n' | sed -n 's/:leader$//p')
	r=$(upload "dist/samuori-$good.samuoriupdate")
	[ "$(echo "$r" | code)" = 201 ] || fail "upload: $r"
	final=$(rollout "$good" 100)
	case $final in succeeded*) ;; *) fail "the rollout ended: $final" ;; esac
	firstHost=$(echo "$final" | sed 's/.*| //' | tr ';' '\n' | head -1 | awk '{print $1}')
	[ -n "$firstHost" ] && [ "$firstHost" != "$leader" ] || fail "the first host $firstHost is not a secondary (leader was $leader)"
	echo "first: $firstHost (leader was $leader)"
	newLeader=$(controllers | tr ' ' '\n' | sed -n 's/:leader$//p')
	[ "$newLeader" = "$firstHost" ] || fail "the leader is $newLeader; want $firstHost, which updated first"
	echo "leader now $newLeader"
	v=$(versions)
	echo "versions: $v"
	for c in $v; do [ "${c#*=}" = "$good" ] || fail "$c is not on $good"; done
	health_all $vms
	echo "CLUSTER UPDATE-ALL TEST PASSED"
}

case ${1:-} in
up) up ;;
update-all) update_all ;;
test) test_cluster ;;
update) update_cluster ;;
down) down ;;
*) usage ;;
esac
