#!/bin/sh
# Applies a signed Samuori release that this host's controller or worker staged in /var/lib/samuori-update, then reports
# the outcome there. Runs as root from samuori-self-update.service (docs/operators/upgrades.md#self-update, ADR 0044).
set -u
dir=/var/lib/samuori-update
signers=/etc/samuori/release-signers
releases=/var/lib/samuori-release
[ -f "$dir/request" ] || exit 0
want=$(head -c 128 "$dir/request" | tr -cd 'A-Za-z0-9.+_-')
case $(uname -m) in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; *) arch=unknown ;; esac
work=$(mktemp -d /var/lib/samuori-update-work.XXXXXX)
trap 'rm -rf "$work"' EXIT

report() {
	detail=$(printf '%s' "$2" | tr -d '"\\' | tr '\n' ' ' | cut -c1-500)
	printf '{"version":"%s","outcome":"%s","detail":"%s"}\n' "$want" "$1" "$detail" >"$dir/result.tmp"
	chmod 0644 "$dir/result.tmp"
	mv "$dir/result.tmp" "$dir/result"
	rm -f "$dir/request" "$dir/release.tar.gz" "$dir/SHA256SUMS" "$dir/SHA256SUMS.sig"
	exit 0
}

[ -n "$want" ] || report failed "the request names no version"
[ -s "$signers" ] || report failed "no release signers are configured in $signers"
[ -s /etc/samuori/install-args ] || report failed "this host was installed before self-update; reinstall it once with install.sh"
args=$(cat /etc/samuori/install-args)
install_from() {
	# One option per line in install-args; no option holds a space.
	# shellcheck disable=SC2086
	"$1/install.sh" --release "$1" $args >"$work/install.log" 2>&1
}
healthy() {
	sleep 15
	if [ -f /etc/systemd/system/samuori-controller.service ]; then
		systemctl is-active --quiet samuori-controller.service
	elif [ -f /etc/systemd/system/samuori-server.service ]; then
		systemctl is-active --quiet samuori-server.service
	else
		systemctl is-active --quiet samuori-worker.service
	fi
}

# A rollback reinstalls the root-owned copy kept before the last update; it is never staged by the service.
if [ "$want" = rollback ]; then
	[ -x "$releases/previous/install.sh" ] || report failed "no previous release is kept on this host"
	if install_from "$releases/previous" && healthy; then
		report succeeded "reinstalled the previous release"
	fi
	report failed "reinstalling the previous release failed: $(tail -n 3 "$work/install.log" 2>/dev/null)"
fi
# Verify copies the staging user cannot change afterwards.
for f in release.tar.gz SHA256SUMS SHA256SUMS.sig; do
	cp "$dir/$f" "$work/$f" 2>/dev/null || report failed "$f was not staged"
done
ssh-keygen -Y verify -f "$signers" -I release@samuori.invalid -n samuori-release -s "$work/SHA256SUMS.sig" <"$work/SHA256SUMS" >/dev/null 2>&1 ||
	report failed "the release signature does not match a key in $signers"
name="samuori-$want-linux-$arch.tar.gz"
expected=$(awk -v n="$name" '$2 == n { print $1 }' "$work/SHA256SUMS")
[ -n "$expected" ] || report failed "the signed checksums list no $name"
[ "$(sha256sum "$work/release.tar.gz" | cut -d' ' -f1)" = "$expected" ] || report failed "the release does not match its signed checksum"
mkdir "$work/x"
tar -xzf "$work/release.tar.gz" -C "$work/x" || report failed "the release archive could not be unpacked"
rel="$work/x/samuori-linux-$arch"
[ -x "$rel/install.sh" ] || report failed "the release holds no installer for $arch"

rm -rf "$releases/previous"
[ -d "$releases/current" ] && cp -a "$releases/current" "$releases/previous"
if install_from "$rel" && healthy; then
	report succeeded "installed $want"
fi
failure=$(tail -n 3 "$work/install.log" 2>/dev/null)
if [ -x "$releases/previous/install.sh" ] && install_from "$releases/previous"; then
	report failed "installing $want failed and the previous release was reinstalled: $failure"
fi
report failed "installing $want failed and the previous release could not be reinstalled: $failure"
