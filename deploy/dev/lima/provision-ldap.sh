#!/bin/sh
# Installs OpenLDAP's slapd in the samuori-dev VM for the LDAP adapter tests, which start their own throwaway server.
# The packaged system service is stopped and disabled. Development use only.
set -eu
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends slapd ldap-utils
sudo systemctl disable --now slapd
# AppArmor confines /usr/sbin/slapd to its packaged paths; the tests run it from temporary directories.
if [ -e /etc/apparmor.d/usr.sbin.slapd ]; then
  sudo ln -sf /etc/apparmor.d/usr.sbin.slapd /etc/apparmor.d/disable/usr.sbin.slapd
  sudo apparmor_parser -R /etc/apparmor.d/usr.sbin.slapd 2>/dev/null || true
fi
/usr/sbin/slapd -VV 2>&1 | head -1
