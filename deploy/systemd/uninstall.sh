#!/bin/sh
# Removes everything the Samuori installers put on this host, including the release folder the installer ran from
# (ADR 0045). Operating-system packages and system logs stay. Runs as root: by hand (sudo samuori uninstall) or from
# samuori-wipe.service when a controller removes this host.
set -u
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
if [ "${1:-}" != --yes ] && [ "${1:-}" != --from-samuori ]; then
	printf 'This deletes Samuori and all its data from this host. Type "wipe" to continue: ' >&2
	read -r answer
	[ "$answer" = wipe ] || { echo "nothing changed" >&2; exit 1; }
fi

source_dir=$(cat /etc/samuori/install-source 2>/dev/null || true)

# samuori-wipe.service may be the unit running this script, so it is never stopped, only removed below.
units="samuori-self-update.path samuori-self-update.service samuori-wipe.path samuori-worker.path samuori-worker.service
samuori-server.service samuori-controller.service samuori-nats-forget.path samuori-nats-forget.service samuori-nats-routes.path
samuori-nats-routes.service samuori-nats.service
samuori-local-registry.service samuori-mirror.service samuori-mirror-ghcr.service samuori-egress-firewall.service samuori-network.service"
for h in $(awk '{ print $1 }' /etc/samuori/registry-mirrors 2>/dev/null); do
	units="$units samuori-mirror@$h.service"
done
units="$units samuori-mirror@.service"
for u in $units; do
	systemctl disable --now "$u" >/dev/null 2>&1 || true
done
for u in $units samuori-wipe.service; do
	rm -f "/etc/systemd/system/$u"
done
rm -rf /etc/systemd/system/samuori-server.service.d /etc/systemd/system/samuori-controller.service.d
systemctl daemon-reload
systemctl reset-failed 'samuori-*' 2>/dev/null || true
nft delete table inet samuori_egress 2>/dev/null || true
ip link delete samuori0 2>/dev/null || true

if id samuori-build >/dev/null 2>&1; then
	loginctl disable-linger samuori-build 2>/dev/null || true
	pkill -KILL -u samuori-build 2>/dev/null || true
fi
for user in samuori samuori-worker samuori-build samuori-registry nats; do
	if id "$user" >/dev/null 2>&1; then
		pkill -KILL -u "$user" 2>/dev/null || true
		userdel "$user" 2>/dev/null || true
	fi
done
for group in samuori-builds samuori-update samuori samuori-worker samuori-registry nats; do
	getent group "$group" >/dev/null && groupdel "$group" 2>/dev/null || true
done
sed -i '/^samuori-build:/d' /etc/subuid /etc/subgid 2>/dev/null || true

for profile in rootlesskit samuori-server; do
	if [ -f "/etc/apparmor.d/usr.local.bin.$profile" ]; then
		apparmor_parser -R "/etc/apparmor.d/usr.local.bin.$profile" 2>/dev/null || true
		rm -f "/etc/apparmor.d/usr.local.bin.$profile"
	fi
done
rm -f /etc/sudoers.d/samuori-worker

rm -rf /etc/samuori /var/lib/samuori /var/lib/samuori-local /var/lib/samuori-worker /var/lib/samuori-build /var/lib/samuori-registry \
	/var/lib/samuori-update /var/lib/samuori-release /var/lib/nats /usr/local/share/samuori /usr/local/lib/samuori
rm -rf /var/lib/samuori-update-work.* /tmp/samuori-bk-* 2>/dev/null || true
rm -f /usr/local/bin/samuori-server /usr/local/bin/samuori-worker /usr/local/bin/samuori /usr/local/bin/samuori-web /usr/local/bin/nats-server \
	/usr/local/bin/buildkitd /usr/local/bin/buildctl /usr/local/bin/buildkit-* /usr/local/bin/rootlesskit \
	/usr/local/bin/rootlessctl /usr/local/bin/rootlesskit-docker-proxy /usr/local/bin/registry /usr/local/bin/syft \
	/usr/local/sbin/samuori-local-enroll /usr/local/sbin/samuori-setup-code /usr/local/sbin/samuori-enroll-worker \
	/usr/local/sbin/samuori-self-update

# The release folder goes only when it still looks like one and is not a system or home directory itself.
release_folder() {
	case $1 in
	"" | / | /root | /home | /opt | /usr | /usr/* | /etc | /etc/* | /var | /var/* | /bin* | /sbin* | /lib* | /boot* | /proc* | /sys* | /dev* | /run*) return 1 ;;
	esac
	[ "$(dirname "$1")" != /home ] && [ -f "$1/install.sh" ] && [ -f "$1/self-update.sh" ] &&
		{ [ -f "$1/samuori-worker" ] || [ -f "$1/samuori-server" ]; }
}
if release_folder "$source_dir"; then
	rm -rf "$source_dir"
fi

rm -f /usr/local/sbin/samuori-uninstall
echo "Samuori removed from this host."
