#!/bin/sh
# Prints the one-time setup code of the all-in-one controller, native or Compose.
set -eu
volume=$(docker volume inspect -f '{{.Mountpoint}}' samuori_samuori-data 2>/dev/null || true)
for f in /var/lib/samuori/bootstrap-token "$volume/bootstrap-token"; do
	if [ -f "$f" ]; then
		exec cat "$f"
	fi
done
echo "no setup code: setup is complete, or the controller has not started yet" >&2
exit 1
