#!/bin/sh
# Enrolls a remote worker installed with install.sh --worker-only: sudo samuori-enroll-worker sha256:<CA pin>, then paste
# the enrollment token shown under Administration > Nodes & workers. samuori-join worker does both in one step.
set -eu
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
case ${1:-} in sha256:*) ;; *) echo "usage: samuori-enroll-worker sha256:<CA pin>" >&2; exit 2 ;; esac
[ ! -t 0 ] || printf 'Enrollment token: ' >&2
read -r token
case $token in samuoriw_*) ;; *) echo "that is not an enrollment token" >&2; exit 2 ;; esac
umask 077
printf '%s' "$token" >/var/lib/samuori-worker/enrollment-token
printf '%s\n' "$1" >/var/lib/samuori-worker/ca-pin
chown samuori-worker:samuori-worker /var/lib/samuori-worker/enrollment-token /var/lib/samuori-worker/ca-pin
chmod 0644 /var/lib/samuori-worker/ca-pin
systemctl restart samuori-worker.service
echo "The worker is enrolling; it appears under Administration > Nodes & workers." >&2
