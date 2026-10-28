#!/bin/sh
# Returns a clean-install VM (default samuori-clean) to its just-installed state, for repeated acceptance runs.
set -eu
vm=${1:-samuori-clean}
limactl shell "$vm" sh -c '
set -e
sudo systemctl stop samuori-worker.path samuori-worker.service
if [ -n "$(cd ~/samuori-release && sudo docker compose ps -q 2>/dev/null)" ]; then
	(cd ~/samuori-release && sudo docker compose down -v >/dev/null 2>&1)
	compose=yes
else
	sudo systemctl stop samuori-server.service samuori-local-registry.service
	compose=no
fi
sudo sh -c "rm -rf /var/lib/samuori/* /var/lib/samuori-local/* /var/lib/samuori-worker/* /var/lib/samuori-registry/local/* /tmp/samuori-bk-*"
sudo install -d -o samuori-worker -g samuori-worker -m 0700 /var/lib/samuori-worker/environments
sudo install -d -o samuori-worker -g samuori-builds -m 2770 /var/lib/samuori-worker/daemons
if [ $compose = yes ]; then
	(cd ~/samuori-release && sudo docker compose up -d --wait >/dev/null 2>&1)
else
	sudo systemctl start samuori-local-registry.service samuori-server.service
fi
sudo systemctl start samuori-worker.path
for _ in $(seq 1 30); do curl -fsS -o /dev/null http://127.0.0.1:8080/api/v1/system/health && exit 0; sleep 1; done
exit 1'
