#!/bin/sh
# Disposable two-node development cluster: controller samuori-clean and remote worker samuori-remote.
# See docs/developers/local-dev.md#two-node-vms.
set -eu

usage() {
	cat <<EOF
Usage: $0 <command>
  up        build the release, recreate both VMs from scratch, install them and set up
            admin/admin1 and dana/dana1 with an enrolled worker (--no-setup skips the setup)
  update    build a signed bundle one patch version ahead (make update), upload it, run the checks and roll it
            out to the controller and the worker through the API (needs make release-key before up)
  down      delete both VMs
  clean     down, then remove dist/ and Lima's download cache
  status    list the VMs
EOF
	exit 2
}

repo=$(cd "$(dirname "$0")/../../.." && pwd)
lima=$repo/deploy/dev/lima
vms="samuori-clean samuori-remote"

exists() { limactl list -q 2>/dev/null | grep -qx "$1"; }

down() {
	for vm in $vms; do
		if exists "$vm"; then
			echo "deleting $vm"
			limactl delete -f "$vm" >/dev/null 2>&1
		fi
	done
}

up() {
	setup=yes
	[ "${1:-}" = --no-setup ] && setup=no
	cd "$repo"
	# Hosts trust the local release key when there is one, so portal updates can be tested (make update).
	[ -f dist/signing/allowed_signers ] && export SAMUORI_RELEASE_SIGNERS="$repo/dist/signing/allowed_signers"
	echo "building the release"
	make release >/dev/null
	down
	echo "creating samuori-clean (controller)"
	limactl start --tty=false --name samuori-clean "$lima/samuori-clean.yml" >/dev/null 2>&1
	"$lima/install-clean.sh" dist/samuori-linux-arm64 samuori-clean \
		--worker-listen 0.0.0.0:8444 \
		--worker-hostnames localhost,127.0.0.1,lima-samuori-clean.internal \
		--no-local-worker >/dev/null
	# Loopback-only development controller: username+1 passwords (SEC-AUTH-10).
	limactl shell samuori-clean sh -c '
		sudo mkdir -p /etc/systemd/system/samuori-server.service.d
		printf "[Service]\nEnvironment=SAMUORI_DEV_WEAK_PASSWORDS=true\n" |
			sudo tee /etc/systemd/system/samuori-server.service.d/dev-weak-passwords.conf >/dev/null
		sudo systemctl daemon-reload
		sudo systemctl restart samuori-server
		for _ in $(seq 1 30); do curl -fs -o /dev/null http://127.0.0.1:8080/api/v1/system/health && exit 0; sleep 1; done
		echo "samuori-server did not become healthy" >&2; exit 1'
	echo "creating samuori-remote (worker)"
	limactl start --tty=false --name samuori-remote "$lima/samuori-remote.yml" >/dev/null 2>&1
	# Only the release folder: the acceptance test installs and enrolls it with samuori-join worker.
	COPYFILE_DISABLE=1 tar --no-xattrs -C dist/samuori-linux-arm64 -czf - . |
		limactl shell samuori-remote sh -c 'rm -rf ~/samuori-release && mkdir -p ~/samuori-release && tar -xzf - -C ~/samuori-release'
	[ -z "${SAMUORI_RELEASE_SIGNERS:-}" ] || limactl shell samuori-remote sh -c 'cat > ~/samuori-release/allowed_signers' <"$SAMUORI_RELEASE_SIGNERS"
	if [ $setup = yes ]; then
		echo "setting up the organization, users and worker"
		SAMUORI_ACCEPTANCE_ADMIN_PASSWORD=admin1 SAMUORI_ACCEPTANCE_DEV_PASSWORD=dana1 make test-acceptance-two-node
		echo "ready: http://127.0.0.1:18480  admin/admin1  dana/dana1"
	else
		echo "ready for setup: http://127.0.0.1:18480  (setup code: limactl shell samuori-clean sudo samuori-setup-code)"
	fi
}

update() {
	cd "$repo"
	[ -f dist/signing/release ] || { echo "no release key: run make release-key, then up" >&2; exit 1; }
	v=0.1.1-dev+$(git rev-parse --short HEAD)
	echo "building the update bundle $v"
	make update VERSION="$v" UPDATE_ARCHES=arm64 >/dev/null
	u=http://127.0.0.1:18480/api/v1 jar=$(mktemp)
	trap 'rm -f "$jar"' EXIT
	curl -fs -c "$jar" -o /dev/null -H 'Content-Type: application/json' -d '{"username":"admin","password":"admin1"}' $u/auth/login
	csrf=$(awk '$6=="samuori_csrf"{print $7}' "$jar")
	call() { curl -s -b "$jar" -c "$jar" -H "X-CSRF-Token: $csrf" "$@"; }
	py() { python3 -c "import json,os,sys; d=json.load(sys.stdin); $1"; }
	echo "uploading"
	call -f -F "bundle=@dist/samuori-$v.samuoriupdate" $u/system/releases >/dev/null || { echo "upload refused" >&2; exit 1; }
	call "$u/system/releases/$v/preflight" | py '
[print("  check %-14s %-7s %s" % (c["name"], c["state"], c["detail"])) for c in d["checks"]]
sys.exit(0 if d["ready"] else 1)' || { echo "the checks did not pass" >&2; exit 1; }
	id=$(call -H 'Content-Type: application/json' -d "{\"version\":\"$v\",\"reason\":\"two-node update test\"}" $u/system/rollouts | py 'print(d["id"])')
	echo "rollout $id started"
	last= state=running
	for _ in $(seq 1 180); do
		sleep 10
		r=$(call "$u/system/rollouts/$id") || continue
		now=$(echo "$r" | py 'print(d["state"], "paused: "+d.get("pauseReason","") if d["paused"] else "", "|", "; ".join(s["name"]+" "+s["state"] for s in d["steps"]))' 2>/dev/null) || continue
		[ "$now" = "$last" ] || { echo "  $now"; last=$now; }
		state=${now%% *}
		case $now in *"paused:"*) echo "the rollout paused" >&2; exit 1 ;; esac
		[ "$state" = running ] || break
	done
	[ "$state" = succeeded ] || { echo "the rollout ended $state" >&2; exit 1; }
	call $u/system/upgrades | V=$v py '
hosts=[("controller", c["hostname"], c["version"]) for c in d["controllers"] if c["live"]]+[("workers", w["count"], w["version"]) for w in d["workers"]]
[print("  %s %s: %s" % h) for h in hosts]
sys.exit(0 if all(h[2]==os.environ["V"] for h in hosts) else 1)' || { echo "not every host runs $v" >&2; exit 1; }
	echo "UPDATE TEST PASSED"
}

case ${1:-} in
up) shift; up "$@" ;;
update) update ;;
down) down ;;
clean)
	down
	rm -rf "$repo/dist"
	limactl prune >/dev/null 2>&1 || true
	echo "removed dist/ and Lima's download cache"
	;;
status) limactl list ;;
*) usage ;;
esac
