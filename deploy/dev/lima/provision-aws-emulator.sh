#!/bin/sh
# Installs moto, a local AWS API emulator (Apache-2.0), into the samuori-dev VM for the AWS adapter tests. No AWS account
# or credentials are used; the tests start their own emulator on loopback.
set -eu
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends python3-venv
sudo python3 -m venv /opt/moto
sudo /opt/moto/bin/pip install --quiet "moto[server]==5.2.3"
sudo ln -sf /opt/moto/bin/moto_server /usr/local/bin/moto_server
moto_server --version 2>/dev/null || /opt/moto/bin/python -c "import moto; print('moto', moto.__version__)"
