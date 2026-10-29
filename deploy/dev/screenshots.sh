#!/bin/sh
# Starts a throwaway samuori-server with its own fresh data directory, seeds example data, captures the screenshot
# tour into docs/screenshots and removes everything again (docs/screenshots/README.md).
# Usage: screenshots.sh <samuori-server> <ui-dist>
set -eu
server=$1 ui=$2
port=${SAMUORI_TOUR_PORT:-18090}
root=$(cd "$(dirname "$0")/../.." && pwd)
dir=$(mktemp -d /tmp/samuori-tour.XXXXXX)
pid=""
cleanup() {
	if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
	rm -rf "$dir"
}
trap cleanup EXIT
trap 'exit 1' INT TERM
if curl -fsS "http://127.0.0.1:$port/api/v1/system/health" >/dev/null 2>&1; then
	echo "port $port is already in use; set SAMUORI_TOUR_PORT" >&2
	exit 1
fi
mkdir "$dir/data"
touch "$dir/data/.samuori-tour"
(umask 077 && printf admin1 >"$dir/admin-password")
"$server" --listen "127.0.0.1:$port" --worker-listen "127.0.0.1:$((port + 1))" --data-dir "$dir/data" --ui-dir "$ui" \
	--bootstrap-organization Example --bootstrap-admin admin --bootstrap-admin-password-file "$dir/admin-password" \
	--dev-weak-passwords=true >"$dir/server.log" 2>&1 &
pid=$!
i=0
until curl -fsS "http://127.0.0.1:$port/api/v1/system/health" >/dev/null 2>&1; do
	i=$((i + 1))
	if [ "$i" -gt 60 ] || ! kill -0 "$pid" 2>/dev/null; then cat "$dir/server.log"; exit 1; fi
	sleep 1
done
mkdir -p "$root/docs/screenshots"
cd "$root/ui"
SAMUORI_TOUR_URL="http://127.0.0.1:$port" SAMUORI_TOUR_DATA="$dir/data" npx playwright test -c playwright.tour.config.ts
