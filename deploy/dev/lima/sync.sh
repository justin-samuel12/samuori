#!/bin/sh
# Copies the working tree (tracked and untracked, not ignored) into the dev VM at ~/samuori.
set -eu
cd "$(dirname "$0")/../../.."
git ls-files -co --exclude-standard -z | COPYFILE_DISABLE=1 tar --no-xattrs --null -T - -czf - | limactl shell samuori-dev sh -c 'rm -rf ~/samuori && mkdir -p ~/samuori && tar -xzf - -C ~/samuori'
