#!/bin/sh
# Installs Prometheus and the OpenTelemetry Collector in the samuori-dev VM for the metrics and tracing interop tests,
# which start their own throwaway processes. Development use only.
set -eu
case "$(uname -m)" in
  aarch64) arch=arm64 promsum=f1f90ec08e849d494ca66c611470afc50192f0355f1a61c33f2cbde02d067823 otelsum=6ae44683515ae0e561acbc9f2a0152dc98ef14ce89035417f95184cffd7cca12 ;;
  x86_64) arch=amd64 promsum=2a542df32eac02ee17b9d844fb2aa1de00dafa5476579ba8a3ba862e9d572ea0 otelsum=f99929987a915d3c6b2c9b15bc4938c5cea903a37a3f49e478024a0fa0772339 ;;
  *) echo "unsupported architecture $(uname -m)"; exit 1 ;;
esac
dl=$(mktemp -d)
cd "$dl"
curl -fsSL -o prometheus.tgz https://github.com/prometheus/prometheus/releases/download/v3.15.0/prometheus-3.15.0.linux-$arch.tar.gz
echo "$promsum  prometheus.tgz" | sha256sum -c -
tar -xzf prometheus.tgz --strip-components=1 prometheus-3.15.0.linux-$arch/prometheus
sudo install -m 0755 prometheus /usr/local/bin/prometheus
curl -fsSL -o otelcol.tgz https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v0.162.0/otelcol_0.162.0_linux_$arch.tar.gz
echo "$otelsum  otelcol.tgz" | sha256sum -c -
tar -xzf otelcol.tgz otelcol
sudo install -m 0755 otelcol /usr/local/bin/otelcol
cd / && rm -rf "$dl"
prometheus --version | head -1
otelcol --version
