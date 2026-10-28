#!/bin/sh
# Installs or upgrades a clustered controller: samuori-server and one NATS JetStream member, no builds
# (docs/operators/high-availability.md). Called by install.sh --role controller.
# Usage: install-controller.sh <release dir> <host> init|join [samuori-server cluster flags]   (upgrade: no action)
#        install-controller.sh <release dir> "" prepare   (prerequisites only)
set -eu
release=$1 host=$2 action=$3
shift 3
fail() { echo "install: $*" >&2; exit 1; }
[ -x "$release/samuori-server" ] && [ -f "$release/ui/index.html" ] || fail "--release must name a directory with samuori-server and ui/index.html"
[ "$action" = prepare ] || [ -n "$host" ] || fail "--host is required: the name other controllers and workers use for this host"
command -v systemctl >/dev/null || fail "systemd is required"
command -v apt-get >/dev/null || fail "this installer supports Debian and Ubuntu"
if [ "$action" = prepare ]; then
	:
elif [ ! -f /etc/samuori/cluster.key ]; then
	[ "$action" = init ] || [ "$action" = join ] || fail "this host is not set up yet: run with init (first controller) or join"
	for port in 8443 8444 4222 6222; do
		ss -ltnH "sport = :$port" | grep -q . && fail "port $port is already in use"
	done
elif [ -n "$action" ]; then
	fail "this host is already a controller; run without init or join to upgrade it"
fi
case $(uname -m) in
x86_64) nats_arch=amd64 nats_sum=bf5edc7b1ce98e885c368cb43a886eb148dc5d0551d99f3cf6141fb5e1954c70 syft_sum=c069905b391cc4c20a5ba65ad5c10be2a7ba074f8ea6ad203e24d14e303dad47 ;;
aarch64) nats_arch=arm64 nats_sum=e2fbab4cad7d3c76e3052eb959570393c9d73ba756b70bf8e9eea61e9ae6d49c syft_sum=dfdf0537610113edbefe1f1fc6548bc957b2d77439636ec824fcf0e10d46d054 ;;
*) fail "unsupported architecture $(uname -m)" ;;
esac

apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl ca-certificates postgresql-client iproute2 openssh-client >/dev/null
dl=$(mktemp -d)
trap 'rm -rf "$dl"' EXIT
curl -fsSL -o "$dl/nats.tgz" "https://github.com/nats-io/nats-server/releases/download/v2.15.1/nats-server-v2.15.1-linux-$nats_arch.tar.gz"
echo "$nats_sum  $dl/nats.tgz" | sha256sum -c --quiet - || fail "checksum mismatch for nats-server"
tar -xzf "$dl/nats.tgz" -C "$dl"
install -m 0755 "$dl"/nats-server-*/nats-server /usr/local/bin/nats-server
# Syft generates the SBOMs that validation records on the controller.
curl -fsSL -o "$dl/syft.tgz" "https://github.com/anchore/syft/releases/download/v1.54.1/syft_1.54.1_linux_$nats_arch.tar.gz"
echo "$syft_sum  $dl/syft.tgz" | sha256sum -c --quiet - || fail "checksum mismatch for syft"
tar -xzf "$dl/syft.tgz" -C "$dl" syft
install -m 0755 "$dl/syft" /usr/local/bin/syft
if [ "$action" = prepare ]; then
	echo "prerequisites installed: postgresql-client, nats-server and Syft; no Samuori binary, user or service yet"
	exit 0
fi
install -m 0755 "$release/samuori-server" /usr/local/bin/samuori-server
rm -rf /usr/local/share/samuori/ui && mkdir -p /usr/local/share/samuori && cp -R "$release/ui" /usr/local/share/samuori/ui && chmod -R a+rX /usr/local/share/samuori
# The discovery sandbox needs user namespaces, which Ubuntu 23.10+ grants per binary (ADR 0013).
if [ -d /etc/apparmor.d ] && command -v apparmor_parser >/dev/null; then
	cat >/etc/apparmor.d/usr.local.bin.samuori-server <<'PROFILE'
abi <abi/4.0>,
include <tunables/global>
profile samuori-server /usr/local/bin/samuori-server flags=(unconfined) {
  userns,
}
PROFILE
	apparmor_parser -r /etc/apparmor.d/usr.local.bin.samuori-server
fi
id samuori >/dev/null 2>&1 || useradd --system --home-dir /var/lib/samuori --shell /usr/sbin/nologin --user-group samuori
usermod -aG samuori-update samuori
id nats >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin --user-group nats
install -d -o samuori -g samuori -m 0750 /var/lib/samuori
install -d -o nats -g nats -m 0750 /var/lib/nats
here=$(cd "$(dirname "$0")" && pwd)
install -m 0755 "$here/setup-code.sh" /usr/local/sbin/samuori-setup-code
install -m 0644 "$here/samuori-controller.service" "$here/samuori-nats.service" "$here/samuori-nats-forget.service" "$here/samuori-nats-forget.path" \
	"$here/samuori-nats-routes.service" "$here/samuori-nats-routes.path" /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now samuori-self-update.path samuori-wipe.path

if [ -n "$action" ]; then
	/usr/local/bin/samuori-server cluster "$action" --host "$host" "$@"
fi
# Members installed before the NATS system account get one now; samuori-nats restarts below (ADR 0045).
/usr/local/bin/samuori-server cluster nats-system
# Flush binaries, units and keys to disk before restarting: a host that loses power right after an upgrade must not
# come back with empty files.
sync
# A controller set up with --nats-url uses an external NATS cluster and runs no member of its own.
if [ -f /etc/samuori/nats/nats.conf ]; then
	systemctl enable samuori-nats.service >/dev/null 2>&1
	systemctl restart samuori-nats.service
	# The controller fills the routes, then turns on route verification once every connected peer is listed (OD-27).
	systemctl enable --now samuori-nats-forget.path samuori-nats-routes.path >/dev/null 2>&1
else
	systemctl disable --now samuori-nats.service samuori-nats-forget.path samuori-nats-routes.path >/dev/null 2>&1 || true
fi
systemctl enable samuori-controller.service >/dev/null 2>&1
systemctl restart samuori-controller.service
code=000
for _ in $(seq 1 90); do
	code=$(curl -sk -o /dev/null -w '%{http_code}' https://127.0.0.1:8443/api/v1/system/health || true)
	[ "$code" = 000 ] || break
	sleep 2
done
[ "$code" != 000 ] || fail "the controller does not answer; see: journalctl -u samuori-controller -n 50, samuori-server preflight --config /etc/samuori/server.yml"
if [ "$code" != 200 ]; then
	echo "The controller answers but is unavailable (health $code). Details: curl -sk https://127.0.0.1:8443/api/v1/system/health"
fi
cat <<MSG

Controller $host is running (https://$host:8443).
- Sign in through the load balancer or this address and open Administration > Servers to see the cluster.
- To add a controller, select "Add a controller" there, run this from the release folder on the new host, then
  accept its join request there after comparing the verification code:
    sudo ./samuori-join controller <code>
- To add a worker, select "Add a worker" there and run on the worker VM: sudo ./samuori-join worker <code>
MSG
