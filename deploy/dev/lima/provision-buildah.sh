#!/bin/sh
# Installs rootless Buildah with runc and netavark in the samuori-dev VM for the Buildah engine tests
# (docs/spikes/buildah-isolation.md). Recommended packages are skipped so the GPL crun and fuse-overlayfs stay out.
# Development use only.
set -eu
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends buildah runc netavark
buildah --version
runc --version | head -1
