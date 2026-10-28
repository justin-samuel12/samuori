#!/bin/sh
# All-in-one install on one dedicated Linux VM (docs/operators/all-in-one.md).
# Usage: sudo ./install.sh --release <dir> --confirm-dedicated-vm [--listen 127.0.0.1:8080] [--no-local-registry]
#        [--server-in-compose]   (samuori-server and the local registry run from compose.yml; the worker stays on the host)
#        [--worker-listen 0.0.0.0:8444 --worker-hostnames samuori.example.com]   (accept remote workers)
#        [--no-local-worker]   (this host runs no builds; pass it on every upgrade too)
#        sudo ./install.sh --release <dir> --confirm-dedicated-vm --worker-only --controller samuori.example.com:8444
#        (a remote build VM: worker parts only; enroll it with samuori-enroll-worker; samuori-join worker does both)
#        sudo ./install.sh --release <dir> --role controller --host ctrl1.example init --database <dsn> --s3-bucket <b> ...
#        (add --nats-url tls://... --nats-ca-file <f> with credentials to use an existing NATS cluster)
#        sudo ./install.sh --release <dir> --role controller --host ctrl2.example join --code samuoric_...
#        (clustered controllers: the first runs init, later ones join with a code from the first and wait for an
#        administrator to accept them; no builds run there; samuori-join controller is the short form)
#        [--confirm-dedicated-vm --keep-local-worker]   (a controller converted from a single server: also installs
#        and upgrades this host's worker; topology convert passes it and install-args keeps it, ADR 0046)
#        [--railpack [--railpack-archive <file>] [--mise-archive <file>]]   (optional: the pinned Railpack and the mise it
#        plans with, checksum-verified, from GitHub or archives the operator downloaded; ADR 0047, docs/operators/providers.md)
#        [--registry-mirror <host>]...   (repeatable: a pull-through mirror for another upstream registry such as
#        mcr.microsoft.com, so strict-egress builds can pull its base images; docs/architecture/egress.md)
#        [--release-signers <allowed_signers>]   (enables self-update from the portal for signed releases, ADR 0044)
#        [--with-kompose]   (installs the checksum-verified Kompose v1.38.0 for the worker's Kubernetes conversions; an
#        administrator still approves it under Build & conversion providers, ADR 0048)
#        [--with-move2kube]   (installs the checksum-verified Move2Kube v0.3.15, unmaintained since March 2025; also
#        approved under Build & conversion providers, ADR 0048)
#        [--external-url <url>] [--s3-bucket <b> --s3-region <r> [--s3-prefix <p>]]   (all-in-one settings)
#        [--organization <name> --admin-username <u> [--admin-email <e>] --admin-password-file <f>]
#        (all-in-one first install: the organization and first administrator; samuori-setup passes these)
# Running it again with a newer release upgrades in place. --prepare-only installs the prerequisites and stops
# before any Samuori binary, user or service (aws-prepare.sh).
# <dir> holds samuori-server, samuori-worker and ui/ (the built web UI); --worker-only needs samuori-worker alone.
set -eu

release=
listen=127.0.0.1:8080
dedicated=no
local_registry=yes
local_worker=yes
keep_local_worker=no
server=systemd
bridge=10.231.0.1
controller=127.0.0.1:8444
server_name=localhost
worker_listen=127.0.0.1:8444
worker_hostnames=localhost,127.0.0.1
role=all-in-one
host=
prepare=no
signers=
external_url='' s3_bucket='' s3_region='' s3_prefix=''
org='' admin='' admin_email='' admin_pw=''
railpack=no railpack_archive= mise_archive=
kompose=no
move2kube=no
registry_mirrors=
# Options a self-update reruns this installer with, one per line; init and join options carry secrets and are not kept.
keep=
k() { keep="$keep$(printf '%s\n' "$@")
"; }
while [ $# -gt 0 ]; do
	case $1 in
	--role) role=$2; k "$1" "$2"; shift 2 ;;
	--host) host=$2; k "$1" "$2"; shift 2 ;;
	init | join) [ "$role" = controller ] || { echo "$1 is for --role controller" >&2; exit 2; }; action=$1; shift; break ;;
	--release) release=$2; shift 2 ;;
	--release-signers) signers=$2; shift 2 ;;
	--listen) listen=$2; k "$1" "$2"; shift 2 ;;
	--confirm-dedicated-vm) dedicated=yes; k "$1"; shift ;;
	--no-local-registry) local_registry=no; k "$1"; shift ;;
	--no-local-worker) local_worker=no; k "$1"; shift ;;
	--keep-local-worker) keep_local_worker=yes; k "$1"; shift ;;
	--server-in-compose) server=compose; k "$1"; shift ;;
	--bridge-address) bridge=$2; k "$1" "$2"; shift 2 ;;
	--worker-only) server=none local_registry=no; k "$1"; shift ;;
	--controller) controller=$2; first=${2%%,*}; server_name=${first%:*}; k "$1" "$2"; shift 2 ;;
	--server-name) server_name=$2; k "$1" "$2"; shift 2 ;;
	--worker-listen) worker_listen=$2; k "$1" "$2"; shift 2 ;;
	--worker-hostnames) worker_hostnames=$2; k "$1" "$2"; shift 2 ;;
	--prepare-only) prepare=yes; shift ;;
	--railpack) railpack=yes; k "$1"; shift ;;
	--railpack-archive) railpack=yes railpack_archive=$2; k --railpack "$1" "$2"; shift 2 ;;
	--mise-archive) railpack=yes mise_archive=$2; k --railpack "$1" "$2"; shift 2 ;;
	--with-kompose) kompose=yes; k "$1"; shift ;;
	--with-move2kube) move2kube=yes; k "$1"; shift ;;
	--registry-mirror) registry_mirrors="$registry_mirrors $2"; k "$1" "$2"; shift 2 ;;
	--external-url) external_url=$2; k "$1" "$2"; shift 2 ;;
	--s3-bucket) s3_bucket=$2; k "$1" "$2"; shift 2 ;;
	--s3-region) s3_region=$2; k "$1" "$2"; shift 2 ;;
	--s3-prefix) s3_prefix=$2; k "$1" "$2"; shift 2 ;;
	--organization) org=$2; shift 2 ;;
	--admin-username) admin=$2; shift 2 ;;
	--admin-email) admin_email=$2; shift 2 ;;
	--admin-password-file) admin_pw=$2; shift 2 ;;
	*) echo "unknown option: $1" >&2; exit 2 ;;
	esac
done

fail() { echo "install: $*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || fail "run as root"

# Self-update from the portal (ADR 0044): the staging directory, the root helper, the signers, the options to rerun
# this installer with, and a copy of this release to fall back to.
self_update_setup() {
	from=$(cd "$(dirname "$0")" && pwd)
	getent group samuori-update >/dev/null || groupadd --system samuori-update
	install -d -o root -g samuori-update -m 2770 /var/lib/samuori-update
	install -d -m 0755 /etc/samuori
	printf '%s' "$keep" >/etc/samuori/install-args
	chmod 0600 /etc/samuori/install-args
	[ -z "$signers" ] || install -m 0644 "$signers" /etc/samuori/release-signers
	install -m 0755 "$from/self-update.sh" /usr/local/sbin/samuori-self-update
	install -m 0755 "$from/uninstall.sh" /usr/local/sbin/samuori-uninstall
	install -m 0644 "$from/samuori-self-update.service" "$from/samuori-self-update.path" "$from/samuori-wipe.service" "$from/samuori-wipe.path" /etc/systemd/system/
	src=$(cd "$release" && pwd)
	# Remember the folder an operator installed from, so removing the host deletes it too; self-updates use a
	# temporary folder and keep the first record.
	case $src in /var/lib/*) ;; *) printf '%s\n' "$src" >/etc/samuori/install-source ;; esac
	if [ "$src" != /var/lib/samuori-release/current ]; then
		mkdir -p /var/lib/samuori-release
		rm -rf /var/lib/samuori-release/current.tmp
		cp -a "$src" /var/lib/samuori-release/current.tmp
		rm -rf /var/lib/samuori-release/current
		mv /var/lib/samuori-release/current.tmp /var/lib/samuori-release/current
	fi
}

if [ -n "$external_url$s3_bucket$s3_region$s3_prefix$org$admin$admin_email$admin_pw" ]; then
	[ "$role" = all-in-one ] && [ "$server" = systemd ] ||
		fail "--external-url, --s3-*, --organization and --admin-* are for a native all-in-one install (a controller takes them after init)"
fi
[ -z "$s3_bucket" ] || [ -n "$s3_region" ] || fail "--s3-bucket needs --s3-region"
for h in $registry_mirrors; do
	case $h in
	docker.io | index.docker.io | registry-1.docker.io) fail "docker.io is always mirrored; drop --registry-mirror $h" ;;
	ghcr.io) fail "--railpack mirrors ghcr.io; use it instead of --registry-mirror ghcr.io" ;;
	esac
	echo "$h" | grep -Eqx '[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+' ||
		fail "--registry-mirror $h: give a lower-case registry host such as mcr.microsoft.com, without scheme, port or path"
done
[ "$keep_local_worker" = no ] || [ "$role" = controller ] || fail "--keep-local-worker is for --role controller"
if [ -n "$org$admin$admin_pw" ]; then
	[ -n "$org" ] && [ -n "$admin" ] && [ -f "$admin_pw" ] ||
		fail "--organization, --admin-username and --admin-password-file go together"
fi

case $role in
all-in-one) ;;
worker) server=none local_registry=no ;;
controller)
	[ $prepare = yes ] && action=prepare
	# A controller that keeps its host's worker installs the worker parts below first, then the controller
	# (ADR 0046, OD-28).
	if [ "$keep_local_worker" = yes ] && [ $prepare = no ]; then
		[ "$local_worker" = yes ] || fail "--keep-local-worker and --no-local-worker contradict each other"
		server=none local_registry=no
	else
		[ $prepare = yes ] || self_update_setup
		exec "$(dirname "$0")/install-controller.sh" "$release" "$host" "${action:-}" "$@"
	fi
	;;
*) fail "--role must be all-in-one, controller or worker" ;;
esac
if [ "$server" = none ]; then
	[ -n "$release" ] && [ -x "$release/samuori-worker" ] || fail "--release must name a directory with samuori-worker"
else
	[ -n "$release" ] && [ -x "$release/samuori-server" ] && [ -x "$release/samuori-worker" ] && [ -f "$release/ui/index.html" ] ||
		fail "--release must name a directory with samuori-server, samuori-worker and ui/index.html"
fi
[ "$server" != none ] || [ "$local_worker" = yes ] || fail "--no-local-worker is for a controller host; a worker host exists to run its worker"
[ "$dedicated" = yes ] || fail "builds of uploaded source need a VM that runs nothing else; confirm with --confirm-dedicated-vm"
systemd-detect-virt --vm --quiet || fail "this host is not a virtual machine; rootless-dedicated-vm needs one (ADR 0027)"
[ -f /sys/fs/cgroup/cgroup.controllers ] || fail "cgroup v2 is required"
command -v apt-get >/dev/null || fail "this installer supports Debian and Ubuntu"
if [ "$server" = compose ]; then
	docker compose version >/dev/null 2>&1 || fail "--server-in-compose needs Docker Engine with the Compose plugin"
fi

case $(uname -m) in
x86_64)
	bk_arch=amd64 rk_arch=x86_64
	bk_sum=14813b1fb2a40082114e6d386c6fc5e7dd5298176ce7ee1a18bc33e73e109ac3
	rk_sum=0e757c8f80510d764df8c9a5f169805580b48d02329f0a200900e261ecec27ed
	reg_sum=40df2224d410f72ae425c3371873b078bbdbda3b8b612be9571f0e6751f3acc8
	syft_sum=c069905b391cc4c20a5ba65ad5c10be2a7ba074f8ea6ad203e24d14e303dad47
	rp_arch=x86_64 rp_sum=248a8f60f36854aabed6124113631078a802de3461660855fc4de9c26065c1fb
	mise_arch=x64 mise_sum=0f95d528ad0b8c54f7022be2ff22a36be149123ff9b27bdaa216d349a9f59c17
	kompose_sum=65a6a720605bead3964e8b22d423a0763de451a236fe03de902e366cf3d9c147
	m2k_sum=5bcda9f36be300a5b161b56e93bb4ca93920fa6e16ebaf34453f0778dddb5c0f
	;;
aarch64)
	bk_arch=arm64 rk_arch=aarch64
	bk_sum=0619ca3e3205a32ea2beb115e0242136e4a0716cc1917ba0ceaa4ef4d52382c9
	rk_sum=7a159e79ef3e65e1f2702ab89537bb79de67b433428328b9626cf75786dbe43a
	reg_sum=09d26f88d2c0f161bd1b8bfc6c123571cc3d291dcca8af5477b3cacc4ec95e73
	syft_sum=dfdf0537610113edbefe1f1fc6548bc957b2d77439636ec824fcf0e10d46d054
	rp_arch=arm64 rp_sum=732ece14eb4f4676a43ebba19431f954edf7b130acb76159ef0f2ff86ac42ac9
	mise_arch=arm64 mise_sum=8f7fa64c3d08ba515aac501f18f6f43c00c381793185380fd1a92c411c00cdde
	kompose_sum=24faf6212ec34d325e4724118098d34b9dd321cda2d9ca22efa0f04cdb909894
	m2k_sum=d2a28dea9886c6d3110870be321fe69f1f5d9335b9031504ca4003e5265977bd
	;;
*) fail "unsupported architecture $(uname -m)" ;;
esac

apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq uidmap slirp4netns nftables curl ca-certificates sudo iproute2 openssh-client >/dev/null

dl=$(mktemp -d)
trap 'rm -rf "$dl"' EXIT
fetch() {
	curl -fsSL -o "$dl/$1" "$2"
	echo "$3  $dl/$1" | sha256sum -c --quiet - || fail "checksum mismatch for $2"
}
fetch buildkit.tgz "https://github.com/moby/buildkit/releases/download/v0.34.0/buildkit-v0.34.0.linux-$bk_arch.tar.gz" "$bk_sum"
fetch rootlesskit.tgz "https://github.com/rootless-containers/rootlesskit/releases/download/v3.3.0/rootlesskit-$rk_arch.tar.gz" "$rk_sum"
fetch registry.tgz "https://github.com/distribution/distribution/releases/download/v3.1.2/registry_3.1.2_linux_$bk_arch.tar.gz" "$reg_sum"
fetch syft.tgz "https://github.com/anchore/syft/releases/download/v1.54.1/syft_1.54.1_linux_$bk_arch.tar.gz" "$syft_sum"
tar -xzf "$dl/buildkit.tgz" -C /usr/local
tar -xzf "$dl/rootlesskit.tgz" -C /usr/local/bin
tar -xzf "$dl/registry.tgz" -C "$dl" registry
tar -xzf "$dl/syft.tgz" -C "$dl" syft
install -m 0755 "$dl/registry" /usr/local/bin/registry
install -m 0755 "$dl/syft" /usr/local/bin/syft
if [ $prepare = yes ]; then
	echo "prerequisites installed: BuildKit, RootlessKit, registry and Syft; no Samuori binary, user or service yet"
	exit 0
fi
install -m 0755 "$release/samuori-worker" /usr/local/bin/
# The web launcher that frontend-and-backend images run (ADR 0055); builds copy it from here.
install -m 0755 "$release/samuori-web" /usr/local/bin/
# Railpack sums are upstream's checksums.txt for v0.43.0; the worker reports the version and runs only a binary
# whose SHA-256 the administrator approved. Mise sums are mise's SHASUMS256.txt for the version Railpack plans with;
# the worker stages it so planning uses the egress proxy (docs/spikes/e2e-providers.md).
if [ "$railpack" = yes ]; then
	rp_url="https://github.com/railwayapp/railpack/releases/download/v0.43.0/railpack-v0.43.0-$rp_arch-unknown-linux-musl.tar.gz"
	if [ -n "$railpack_archive" ]; then
		cp "$railpack_archive" "$dl/railpack.tgz"
		echo "$rp_sum  $dl/railpack.tgz" | sha256sum -c --quiet - || fail "$railpack_archive is not Railpack v0.43.0 for $rp_arch"
	else
		fetch railpack.tgz "$rp_url" "$rp_sum"
	fi
	tar -xzf "$dl/railpack.tgz" -C "$dl" railpack
	install -d -m 0755 /usr/local/lib/samuori/providers/railpack
	install -m 0755 "$dl/railpack" /usr/local/lib/samuori/providers/railpack/railpack
	echo v0.43.0 >/usr/local/lib/samuori/providers/railpack/VERSION
	if [ -n "$mise_archive" ]; then
		cp "$mise_archive" "$dl/mise.tgz"
		echo "$mise_sum  $dl/mise.tgz" | sha256sum -c --quiet - || fail "$mise_archive is not mise v2026.10.14 for $mise_arch (musl)"
	else
		fetch mise.tgz "https://github.com/jdx/mise/releases/download/v2026.10.14/mise-v2026.10.14-linux-$mise_arch-musl.tar.gz" "$mise_sum"
	fi
	tar -xzf "$dl/mise.tgz" -C "$dl" mise/bin/mise
	install -m 0755 "$dl/mise/bin/mise" /usr/local/lib/samuori/providers/railpack/mise
	echo "Railpack v0.43.0 installed; approve binary-$bk_arch sha256:$(sha256sum /usr/local/lib/samuori/providers/railpack/railpack | cut -d' ' -f1)"
fi
# Kompose runs only inside a worker's conversion step, never on this host directly (ADR 0048).
if [ $kompose = yes ]; then
	[ "$local_worker" = yes ] || fail "--with-kompose is for a host that runs a worker"
	fetch kompose "https://github.com/kubernetes/kompose/releases/download/v1.38.0/kompose-linux-$bk_arch" "$kompose_sum"
	install -d -m 0755 /usr/local/lib/samuori/providers/kompose
	install -m 0755 "$dl/kompose" /usr/local/lib/samuori/providers/kompose/kompose
	echo v1.38.0 >/usr/local/lib/samuori/providers/kompose/VERSION
fi
# The sum is the release tarball's published .sha256sum; the binary inside is statically linked.
if [ $move2kube = yes ]; then
	[ "$local_worker" = yes ] || fail "--with-move2kube is for a host that runs a worker"
	fetch move2kube.tgz "https://github.com/konveyor/move2kube/releases/download/v0.3.15/move2kube-v0.3.15-linux-$bk_arch.tar.gz" "$m2k_sum"
	tar -xzf "$dl/move2kube.tgz" -C "$dl" move2kube/move2kube
	install -d -m 0755 /usr/local/lib/samuori/providers/move2kube
	install -m 0755 "$dl/move2kube/move2kube" /usr/local/lib/samuori/providers/move2kube/move2kube
	echo v0.3.15 >/usr/local/lib/samuori/providers/move2kube/VERSION
	echo "Move2Kube v0.3.15 installed; approve binary-$bk_arch sha256:$(sha256sum /usr/local/lib/samuori/providers/move2kube/move2kube | cut -d' ' -f1)"
fi
if [ "$server" != none ]; then
	install -m 0755 "$release/samuori-server" /usr/local/bin/
	rm -rf /usr/local/share/samuori/ui
	mkdir -p /usr/local/share/samuori
	cp -R "$release/ui" /usr/local/share/samuori/ui
	chmod -R a+rX /usr/local/share/samuori
fi

# Ubuntu 23.10+ restricts unprivileged user namespaces; grant them per binary, never host-wide (ADR 0013).
if [ -d /etc/apparmor.d ] && command -v apparmor_parser >/dev/null; then
	for bin in rootlesskit samuori-server; do
		cat >/etc/apparmor.d/usr.local.bin.$bin <<EOF
abi <abi/4.0>,
include <tunables/global>
profile $bin /usr/local/bin/$bin flags=(unconfined) {
  userns,
}
EOF
		apparmor_parser -r /etc/apparmor.d/usr.local.bin.$bin
	done
fi

getent group samuori-builds >/dev/null || groupadd --system samuori-builds
id samuori >/dev/null 2>&1 || useradd --system --home-dir /var/lib/samuori --shell /usr/sbin/nologin --user-group samuori
id samuori-worker >/dev/null 2>&1 || useradd --system --home-dir /var/lib/samuori-worker --shell /usr/sbin/nologin --user-group --groups samuori-builds samuori-worker
id samuori-build >/dev/null 2>&1 || useradd --system --create-home --home-dir /var/lib/samuori-build --shell /usr/sbin/nologin --gid samuori-builds samuori-build
id samuori-registry >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin --user-group samuori-registry
self_update_setup
usermod -aG samuori-update samuori
usermod -aG samuori-update samuori-worker
grep -q '^samuori-build:' /etc/subuid || usermod --add-subuids 300000-365535 --add-subgids 300000-365535 samuori-build
# systemd-logind on a fresh Ubuntu 24.04 VM was once seen not answering D-Bus; restarting it recovered.
if ! timeout 30 loginctl enable-linger samuori-build; then
	echo "systemd-logind did not answer; restarting it once" >&2
	systemctl restart systemd-logind
	timeout 30 loginctl enable-linger samuori-build || fail "systemd-logind does not answer: loginctl enable-linger samuori-build failed"
fi

cat >/etc/sudoers.d/samuori-worker <<EOF
Defaults:samuori-worker !use_pty
samuori-worker ALL=(samuori-build) NOPASSWD: /usr/local/bin/rootlesskit, $(command -v pkill), $(command -v env)
EOF
chmod 0440 /etc/sudoers.d/samuori-worker
visudo -cq

install -d -o samuori -g samuori -m 0750 /var/lib/samuori
install -d -o samuori-worker -g samuori-builds -m 0710 /var/lib/samuori-worker
install -d -o samuori-worker -g samuori-worker -m 0700 /var/lib/samuori-worker/environments
install -d -o samuori-worker -g samuori-builds -m 2770 /var/lib/samuori-worker/daemons
install -d -o samuori-registry -g samuori-registry -m 0750 /var/lib/samuori-registry/mirror /var/lib/samuori-registry/local
[ "$railpack" = no ] || install -d -o samuori-registry -g samuori-registry -m 0750 /var/lib/samuori-registry/mirror-ghcr
install -d -m 0755 /etc/samuori
# The controller hands the host worker its single-use enrollment token here; root copies it to the worker.
case $server in
compose) install -d -o 65532 -g 65532 -m 0700 /var/lib/samuori-local ;;
systemd) install -d -o samuori -g samuori -m 0700 /var/lib/samuori-local ;;
none) [ "$keep_local_worker" = no ] || install -d -o samuori -g samuori -m 0700 /var/lib/samuori-local ;;
esac

[ "$server" = none ] || cat >/etc/samuori/server.yml <<EOF
role: all-in-one
listen: $listen
dataDir: /var/lib/samuori
log:
  format: json
ui:
  dir: /usr/local/share/samuori/ui
worker:
  listen: $worker_listen
  hostnames: [$(echo "$worker_hostnames" | sed 's/,/, /g')]
EOF
[ "$server" = none ] || [ "$local_worker" = no ] || cat >>/etc/samuori/server.yml <<EOF
localWorker:
  dir: /var/lib/samuori-local
EOF
yq() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/''/g")"; }
[ -z "$external_url" ] || echo "externalUrl: $(yq "$external_url")" >>/etc/samuori/server.yml
if [ -n "$s3_bucket" ]; then
	printf 'storage:\n  s3:\n    bucket: %s\n    region: %s\n' "$(yq "$s3_bucket")" "$(yq "$s3_region")" >>/etc/samuori/server.yml
	[ -z "$s3_prefix" ] || printf '    prefix: %s\n' "$(yq "$s3_prefix")" >>/etc/samuori/server.yml
fi
# The first administrator is created on the first start; samuori-setup removes the password file once setup is done.
if [ -n "$admin" ]; then
	install -o samuori -g samuori -m 0600 "$admin_pw" /etc/samuori/admin-password
	printf 'bootstrap:\n  organizationName: %s\n  adminUsername: %s\n  adminPasswordFile: /etc/samuori/admin-password\n' "$(yq "$org")" "$(yq "$admin")" >>/etc/samuori/server.yml
	[ -z "$admin_email" ] || printf '  adminEmail: %s\n' "$(yq "$admin_email")" >>/etc/samuori/server.yml
fi

cat >/etc/samuori/mirror.yml <<EOF
version: 0.1
storage:
  filesystem:
    rootdirectory: /var/lib/samuori-registry/mirror
http:
  addr: $bridge:5000
proxy:
  remoteurl: https://registry-1.docker.io
EOF
# Railpack's frontend, builder and runtime images come from ghcr.io, which strict egress reaches only through a mirror.
mirrors="docker.io=$bridge:5000" egress_allow="$bridge:3128,$bridge:5000"
if [ "$railpack" = yes ]; then
	mirrors="$mirrors,ghcr.io=$bridge:5002" egress_allow="$egress_allow,$bridge:5002"
	cat >/etc/samuori/mirror-ghcr.yml <<EOF
version: 0.1
storage:
  filesystem:
    rootdirectory: /var/lib/samuori-registry/mirror-ghcr
http:
  addr: $bridge:5002
proxy:
  remoteurl: https://ghcr.io
EOF
fi
# Each --registry-mirror upstream keeps the port it was first given (from 5003), so reruns and self-updates leave
# BuildKit's mirror list unchanged.
mirror_port() { printf '%s' "$1" | awk -v h="$2" '$1 == h { print $2 }'; }
port_used() { case " $used_ports " in *" $1 "*) return 0 ;; esac; return 1; }
old_mirrors=$(cat /etc/samuori/registry-mirrors 2>/dev/null || true)
extra_mirrors= used_ports=
for h in $registry_mirrors; do
	[ -z "$(mirror_port "$extra_mirrors" "$h")" ] || continue
	p=$(mirror_port "$old_mirrors" "$h")
	extra_mirrors="$extra_mirrors$h ${p:-new}
"
	used_ports="$used_ports ${p:-}"
done
for h in $(printf '%s' "$extra_mirrors" | awk '{ print $1 }'); do
	p=$(mirror_port "$extra_mirrors" "$h")
	if [ "$p" = new ]; then
		p=5003
		while port_used "$p"; do p=$((p + 1)); done
		used_ports="$used_ports $p"
		extra_mirrors=$(printf '%s' "$extra_mirrors" | awk -v h="$h" -v p="$p" '$1 == h { $2 = p } { print }')
		extra_mirrors="$extra_mirrors
"
	fi
	install -d -o samuori-registry -g samuori-registry -m 0750 "/var/lib/samuori-registry/mirror-$h"
	cat >"/etc/samuori/mirror-$h.yml" <<EOF
version: 0.1
storage:
  filesystem:
    rootdirectory: /var/lib/samuori-registry/mirror-$h
http:
  addr: $bridge:$p
proxy:
  remoteurl: https://$h
EOF
	mirrors="$mirrors,$h=$bridge:$p" egress_allow="$egress_allow,$bridge:$p"
done
printf '%s' "$extra_mirrors" >/etc/samuori/registry-mirrors

cat >/etc/samuori/local-registry.yml <<EOF
version: 0.1
storage:
  filesystem:
    rootdirectory: /var/lib/samuori-registry/local
  delete:
    enabled: true
http:
  addr: 127.0.0.1:5001
EOF

here=$(cd "$(dirname "$0")" && pwd)
# The all-in-one worker is "local"; a remote worker is named after its host (lowercase letters, digits, dashes).
worker_name=local
if [ "$server" = none ] && [ "$keep_local_worker" = no ]; then
	worker_name=$(hostname -s | tr 'A-Z_.' 'a-z--' | cut -c1-63)
fi
# Upgrades stop the worker before the firewall is reapplied, so builds never run without it.
systemctl stop samuori-worker.path samuori-worker.service 2>/dev/null || true
rm -f /etc/samuori/worker.env
install -m 0755 "$here/local-enroll.sh" /usr/local/sbin/samuori-local-enroll
install -m 0755 "$here/setup-code.sh" /usr/local/sbin/samuori-setup-code
install -m 0755 "$here/enroll-worker.sh" /usr/local/sbin/samuori-enroll-worker
for unit in "$here"/*.service "$here"/*.path; do
	case $(basename "$unit") in samuori-controller.service | samuori-nats.service) continue ;; esac
	sed -e "s/@MIRRORS@/$mirrors/g" -e "s/@EGRESSALLOW@/$egress_allow/g" -e "s/@BRIDGE@/$bridge/g" -e "s/@CONTROLLER@/$controller/g" -e "s/@SERVERNAME@/$server_name/g" -e "s/@WORKERNAME@/$worker_name/g" "$unit" >"/etc/systemd/system/$(basename "$unit")"
done
if [ "$server" != systemd ]; then
	systemctl disable --now samuori-server.service samuori-local-registry.service 2>/dev/null || true
	rm -f /etc/systemd/system/samuori-server.service /etc/systemd/system/samuori-local-registry.service
fi
# Flush everything to disk before services restart, so a power loss right after an upgrade leaves no empty files.
sync
systemctl daemon-reload
systemctl enable samuori-network.service samuori-egress-firewall.service samuori-mirror.service
systemctl enable --now samuori-self-update.path samuori-wipe.path
if [ "$local_worker" = yes ]; then
	systemctl enable samuori-worker.service samuori-worker.path
else
	systemctl disable --now samuori-worker.service samuori-worker.path 2>/dev/null || true
fi
systemctl restart samuori-network.service samuori-egress-firewall.service samuori-mirror.service
if [ "$railpack" = yes ]; then
	systemctl enable samuori-mirror-ghcr.service
	systemctl restart samuori-mirror-ghcr.service
else
	systemctl disable --now samuori-mirror-ghcr.service 2>/dev/null || true
fi
for h in $(printf '%s' "$old_mirrors" | awk '{ print $1 }'); do
	[ -z "$(mirror_port "$extra_mirrors" "$h")" ] || continue
	systemctl disable --now "samuori-mirror@$h.service" 2>/dev/null || true
	rm -f "/etc/samuori/mirror-$h.yml"
done
for h in $(printf '%s' "$extra_mirrors" | awk '{ print $1 }'); do
	systemctl enable "samuori-mirror@$h.service"
	systemctl restart "samuori-mirror@$h.service"
done
if [ "$server" = none ]; then
	:
elif [ "$server" = compose ]; then
	(cd "$here" && docker compose up -d --build --wait)
else
	systemctl enable samuori-server.service
	systemctl restart samuori-server.service
	if [ "$local_registry" = yes ]; then
		systemctl enable samuori-local-registry.service
		systemctl restart samuori-local-registry.service
	fi
fi
if [ "$local_worker" = yes ]; then
	systemctl start samuori-worker.path
	if [ -f /var/lib/samuori-worker/worker.crt ]; then
		systemctl start samuori-worker.service
	fi
fi

if [ "$keep_local_worker" = yes ]; then
	[ "${action:-}" != init ] || set -- "$@" --local-worker-dir /var/lib/samuori-local
	exec "$here/install-controller.sh" "$release" "$host" "${action:-}" "$@"
fi
if [ "$server" = none ]; then
	[ -z "${SAMUORI_JOINING:-}" ] || exit 0
	cat <<EOF

The worker is installed and will connect to $controller once enrolled. In the controller's UI, under
Administration > Servers, select "Add a worker", then run here:
  sudo samuori-enroll-worker <CA pin>     (paste the token when asked)
Next time, sudo ./samuori-join worker <code> installs and enrolls in one step.
EOF
	exit 0
fi
for _ in $(seq 1 60); do
	curl -fs -o /dev/null "http://$listen/api/v1/system/health" && break
	sleep 1
done
cat <<EOF

Samuori is running on http://$listen.
$(if [ -n "$admin" ]; then echo "1. Sign in as $admin."; else echo "1. Open it and complete setup with the one-time code from: sudo samuori-setup-code"; fi)
$(if [ "$local_worker" = yes ]; then cat <<MSG
   The local worker then enrolls by itself in the pool "local".
2. Under Administration > Nodes & workers, select "Enable pool" for the "local" pool. The worker then runs its
   self-test; uploaded source builds once it passes.
MSG
else cat <<MSG
   This host runs no builds (--no-local-worker).
2. Under Administration > Servers, select "Add a worker" and run sudo ./samuori-join worker <code> from the release
   folder on each worker host, then assign it to a pool. Uploaded source builds once a worker passes its
   self-test for its pool.
MSG
fi)
3. Under Administration > Registries, add the local registry 127.0.0.1:5001 (plain HTTP, private network).
EOF
exit 0
