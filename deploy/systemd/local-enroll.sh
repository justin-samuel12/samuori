#!/bin/sh
# Runs as root before samuori-worker starts: hands the controller's single-use local enrollment token and CA pin to the
# worker (ADR 0032). The controller offers a token only while no valid local worker identity exists.
set -eu
src=/var/lib/samuori-local
dst=/var/lib/samuori-worker
[ -f "$src/enrollment-token" ] && [ -f "$src/ca-pin" ] || exit 0
install -o samuori-worker -g samuori-worker -m 0600 "$src/enrollment-token" "$dst/enrollment-token"
install -o samuori-worker -g samuori-worker -m 0644 "$src/ca-pin" "$dst/ca-pin"
