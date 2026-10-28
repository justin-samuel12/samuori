#!/bin/sh
# Lints the chart, renders it with a complete example and validates every manifest against the Kubernetes schemas
# offline; incomplete settings must fail to render. Needs helm and kubeconform (deploy/dev/lima/provision-helm.sh).
set -eu
chart=$(cd "$(dirname "$0")/../../deploy/helm/samuori" && pwd)
values=$(dirname "$0")/example-values.yml
helm lint --strict "$chart" -f "$values"
helm template samuori "$chart" -f "$values" >/tmp/samuori-chart.yml
kubeconform -strict -summary -kubernetes-version 1.34.0 /tmp/samuori-chart.yml
helm template samuori "$chart" -f "$values" --set backup.enabled=true --set backup.recipientsSecret.name=samuori-backup --set metrics.enabled=true --set metrics.tokenSecret.name=samuori-metrics \
	| kubeconform -strict -summary -kubernetes-version 1.34.0 -
for missing in image.digest tls.secretName database.dsnSecret.name clusterKeySecret.name messaging.urls worker.hostnames externalUrl; do
	if helm template samuori "$chart" -f "$values" --set "$missing=" >/dev/null 2>&1; then
		echo "the chart rendered without $missing"
		exit 1
	fi
done
if helm template samuori "$chart" -f "$values" --set storage.s3.bucket= >/dev/null 2>&1; then
	echo "the chart rendered without shared blob storage"
	exit 1
fi
grep -q 'runAsNonRoot: true' /tmp/samuori-chart.yml
grep -q 'readOnlyRootFilesystem: true' /tmp/samuori-chart.yml
! grep -q 'privileged: true' /tmp/samuori-chart.yml
! grep -qi 'docker.sock' /tmp/samuori-chart.yml
# The controller must accept the configuration the chart renders.
awk '/server.yml: \|/{f=1;next} f&&/^  [a-z]/{f=0} f{sub(/^    /,"");print}' /tmp/samuori-chart.yml >/tmp/samuori-server.yml
SAMUORI_DATABASE=postgres://samuori@db.example/samuori "${SAMUORI_SERVER:-samuori-server}" check-config --config /tmp/samuori-server.yml
echo "chart checks passed"
