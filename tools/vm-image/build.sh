#!/bin/sh
# Builds the vm-per-job guest image bundle (docs/operators/vm-per-job.md): an Ubuntu rootfs with rootless BuildKit
# and the samuori-worker guest agent, the kernel, and manifest.json, whose digest workers pin.
# Usage (as root on a Linux host): tools/vm-image/build.sh <samuori-worker binary> <output dir>
set -eu
export LC_ALL=C
worker=$1
out=$2
suite=${SAMUORI_GUEST_SUITE:-resolute}
mirror=${SAMUORI_GUEST_MIRROR:-http://ports.ubuntu.com/ubuntu-ports}
case "$(uname -m)" in
  aarch64) arch=arm64 rkarch=aarch64 console=ttyAMA0 bksum=0619ca3e3205a32ea2beb115e0242136e4a0716cc1917ba0ceaa4ef4d52382c9 rksum=7a159e79ef3e65e1f2702ab89537bb79de67b433428328b9626cf75786dbe43a ;;
  x86_64) arch=amd64 rkarch=x86_64 console=ttyS0 mirror=${SAMUORI_GUEST_MIRROR:-http://archive.ubuntu.com/ubuntu} bksum=14813b1fb2a40082114e6d386c6fc5e7dd5298176ce7ee1a18bc33e73e109ac3 rksum=0e757c8f80510d764df8c9a5f169805580b48d02329f0a200900e261ecec27ed ;;
  *) echo "unsupported architecture $(uname -m)"; exit 1 ;;
esac
[ "$(id -u)" = 0 ] || { echo "run as root"; exit 1; }
for t in mmdebstrap mkfs.ext4 zstd curl sha256sum; do command -v $t >/dev/null || { echo "missing $t"; exit 1; }; done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
root=$work/root
cd "$work"
curl -fsSL -o buildkit.tgz https://github.com/moby/buildkit/releases/download/v0.34.0/buildkit-v0.34.0.linux-$arch.tar.gz
echo "$bksum  buildkit.tgz" | sha256sum -c -
curl -fsSL -o rootlesskit.tgz https://github.com/rootless-containers/rootlesskit/releases/download/v3.3.0/rootlesskit-$rkarch.tar.gz
echo "$rksum  rootlesskit.tgz" | sha256sum -c -

mmdebstrap --variant=minbase --components=main,universe \
  --include=systemd,systemd-sysv,udev,dbus,dbus-user-session,libpam-systemd,uidmap,sudo,nftables,iproute2,procps,util-linux-extra,ca-certificates,kmod,e2fsprogs,apparmor,slirp4netns,linux-image-virtual \
  --dpkgopt='path-exclude=/usr/share/doc/*' --dpkgopt='path-exclude=/usr/share/man/*' --dpkgopt='path-exclude=/usr/share/locale/*' \
  "$suite" "$root" "deb $mirror $suite main universe" "deb $mirror $suite-updates main universe" "deb $mirror $suite-security main universe"

kver=$(ls "$root/lib/modules" | head -n1)
tar -xzf buildkit.tgz -C "$root/usr/local"
tar -xzf rootlesskit.tgz -C "$root/usr/local/bin"
install -m 0755 "$worker" "$root/usr/local/bin/samuori-worker"

chroot "$root" groupadd --system samuori
chroot "$root" useradd --system --create-home --shell /usr/sbin/nologin --gid samuori samuori-build
echo "samuori-build:300000:65536" > "$root/etc/subuid"
echo "samuori-build:300000:65536" > "$root/etc/subgid"
mkdir -p "$root/var/lib/systemd/linger" && touch "$root/var/lib/systemd/linger/samuori-build"
install -d -m 0700 "$root/var/lib/samuori" "$root/var/lib/samuori/envs"
install -d -m 2770 -g "$(chroot "$root" getent group samuori | cut -d: -f3)" "$root/var/lib/samuori/daemons"
install -d "$root/overlay" "$root/newroot" "$root/oldroot" "$root/usr/lib/samuori" "$root/var/log/journal"
echo samuori-guest > "$root/etc/hostname"
: > "$root/etc/machine-id"
printf 'overlay\nvmw_vsock_virtio_transport\n' > "$root/etc/modules-load.d/samuori.conf"

cat > "$root/etc/apparmor.d/usr.local.bin.rootlesskit" <<'PROFILE'
abi <abi/4.0>,
include <tunables/global>
profile rootlesskit /usr/local/bin/rootlesskit flags=(unconfined) {
  userns,
}
PROFILE

# The image is read-only; every VM gets a fresh overlay on its own scratch disk, so nothing outlives the VM.
cat > "$root/usr/lib/samuori/overlay-init" <<'INIT'
#!/bin/sh
mount -t proc proc /proc
off() {
  echo "samuori overlay-init: $1; powering off"
  [ -e /proc/sysrq-trigger ] || mount -t proc proc /proc
  echo 1 > /proc/sys/kernel/sysrq; echo o > /proc/sysrq-trigger; sleep 60
}
mount -t sysfs sys /sys || off "sysfs"
mount -t devtmpfs dev /dev 2>/dev/null
modprobe overlay || off "overlay module"
mkfs.ext4 -q -F -E nodiscard -O ^has_journal /dev/vdb || off "formatting the scratch disk"
mount -o noatime /dev/vdb /overlay || off "mounting the scratch disk"
mkdir -p /overlay/upper /overlay/work
mount -t overlay overlay -o lowerdir=/,upperdir=/overlay/upper,workdir=/overlay/work /newroot || off "overlay mount"
for m in proc sys dev; do mount --move /$m /newroot/$m || off "moving /$m"; done
cd /newroot && pivot_root . oldroot || off "pivot_root"
exec chroot . /lib/systemd/systemd
INIT
chmod 0755 "$root/usr/lib/samuori/overlay-init"

cat > "$root/etc/systemd/system/samuori-guest-agent.service" <<'UNIT'
[Unit]
Description=Samuori vm-per-job guest agent
After=systemd-modules-load.service apparmor.service

[Service]
ExecStart=/usr/local/bin/samuori-worker guest-agent
Restart=no

[Install]
WantedBy=multi-user.target
UNIT
chroot "$root" systemctl enable samuori-guest-agent.service >/dev/null
for u in getty@tty1.service serial-getty@$console.service systemd-timesyncd.service apt-daily.timer apt-daily-upgrade.timer e2scrub_reap.service fstrim.timer motd-news.timer; do
  chroot "$root" systemctl mask "$u" >/dev/null 2>&1 || true
done

# Cloud Hypervisor boots an uncompressed kernel; Ubuntu ships it inside a compressed EFI image.
vmlinuz=$root/boot/vmlinuz-$kver
for off in $(grep -obUaP '\x28\xb5\x2f\xfd' "$vmlinuz" | cut -d: -f1); do
  tail -c +$((off + 1)) "$vmlinuz" | zstd -dcq 2>/dev/null > "$work/kernel" || true
  if [ "$arch" = arm64 ] && [ "$(dd if="$work/kernel" bs=1 skip=56 count=4 2>/dev/null)" = "ARMd" ]; then break; fi
  if [ "$arch" = amd64 ] && [ "$(dd if="$work/kernel" bs=1 skip=1 count=3 2>/dev/null)" = "ELF" ]; then break; fi
  rm -f "$work/kernel"
done
[ -s "$work/kernel" ] || { echo "could not extract the kernel from $vmlinuz"; exit 1; }
rm -rf "$root/boot"/* "$root/var/cache/apt"/* "$root/var/lib/apt/lists"/*

size=$(( $(du -sm "$root" | cut -f1) * 13 / 10 + 64 ))
mkdir -p "$out"
rm -f "$out/rootfs.img"
mkfs.ext4 -q -L samuori-guest -d "$root" "$out/rootfs.img" "${size}M"
install -m 0644 "$work/kernel" "$out/kernel"
chmod 0644 "$out/rootfs.img"
entry() { printf '{"path": "%s", "digest": "sha256:%s", "size": %s}' "$1" "$(sha256sum "$out/$1" | cut -d' ' -f1)" "$(stat -c %s "$out/$1")"; }
cat > "$out/manifest.json" <<JSON
{
  "arch": "$arch",
  "kernel": $(entry kernel),
  "rootfs": $(entry rootfs.img),
  "cmdline": "console=$console root=/dev/vda ro rootfstype=ext4 init=/usr/lib/samuori/overlay-init quiet panic=0",
  "tools": {"ubuntu": "$suite", "kernel": "$kver", "buildkit": "v0.34.0", "rootlesskit": "v3.3.0", "samuori-worker": "$("$worker" --version 2>/dev/null | head -n1 || echo unknown)"}
}
JSON
echo "guest image: samuori-guest@sha256:$(sha256sum "$out/manifest.json" | cut -d' ' -f1)"
