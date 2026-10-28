#!/bin/sh
# Runs samuori-server on this machine for UI work, keeping state in <dir> across restarts (docs/developers/local-dev.md).
# The first start creates the organization "Dev" and the administrator admin / admin1 (SAMUORI_DEV_ADMIN_PASSWORD
# overrides it). Short passwords such as <username>1 are allowed only because the server listens on 127.0.0.1.
set -eu
dir=$1 ui=$2
mkdir -p "$dir/data"
pw="$dir/admin-password"
(umask 077 && printf '%s' "${SAMUORI_DEV_ADMIN_PASSWORD:-admin1}" >"$pw")
echo "Samuori: http://127.0.0.1:8080   sign in as admin / $(cat "$pw")   (state: $dir/data; reset with make dev-reset)"
exec "$dir/samuori-server" --listen 127.0.0.1:8080 --data-dir "$dir/data" --ui-dir "$ui" \
	--bootstrap-organization Dev --bootstrap-admin admin --bootstrap-admin-password-file "$pw" \
	--dev-weak-passwords=true
