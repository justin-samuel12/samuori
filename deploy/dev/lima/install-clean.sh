#!/bin/sh
# Copies a release directory into a clean Lima VM (default samuori-clean) and runs the all-in-one installer there.
# Usage: install-clean.sh <release-dir> [vm] [installer options...]
# SAMUORI_RELEASE_SIGNERS=<allowed_signers> also installs that file as the host's release signers.
set -eu
release=$1
vm=${2:-samuori-clean}
[ $# -ge 2 ] && shift 2 || shift 1
COPYFILE_DISABLE=1 tar --no-xattrs -C "$release" -czf - . | limactl shell "$vm" sh -c 'rm -rf ~/samuori-release && mkdir -p ~/samuori-release && tar -xzf - -C ~/samuori-release'
if [ -n "${SAMUORI_RELEASE_SIGNERS:-}" ]; then
	limactl shell "$vm" sh -c 'cat > ~/allowed_signers' <"$SAMUORI_RELEASE_SIGNERS"
	set -- --release-signers "$(limactl shell "$vm" sh -c 'echo ~/allowed_signers')" "$@"
fi
limactl shell "$vm" sh -c 'cd ~/samuori-release && sudo ./install.sh --release . --confirm-dedicated-vm "$@"' sh "$@"
