#!/bin/sh
# Prepares a vanilla Ubuntu EC2 instance for Samuori without installing Samuori (docs/operators/aws.md#prepare-hosts).
# Usage: sudo ./aws-prepare.sh controller [--region <r>] [--database-host <host>[:5432]]
#        sudo ./aws-prepare.sh worker --confirm-dedicated-vm [--controllers <c1>,<c2>,<c3>]
#        add --not-ec2 to skip the EC2-only checks on a local VM
set -eu

here=$(cd "$(dirname "$0")" && pwd)
role=${1:-}
[ $# -gt 0 ] && shift
region='' database='' controllers='' dedicated='' ec2=yes
while [ $# -gt 0 ]; do
	case $1 in
	--region) region=$2; shift 2 ;;
	--database-host) database=$2; shift 2 ;;
	--controllers) controllers=$2; shift 2 ;;
	--confirm-dedicated-vm) dedicated=--confirm-dedicated-vm; shift ;;
	--not-ec2) ec2=no; shift ;;
	*) echo "unknown option: $1" >&2; exit 2 ;;
	esac
done

problems=0
ok() { echo "  ok    $*"; }
warn() { echo "  warn  $*"; }
bad() { echo "  FAIL  $*"; problems=$((problems + 1)); }
fail() { echo "aws-prepare: $*" >&2; exit 1; }

case $role in
controller) binary=samuori-server ;;
worker) binary=samuori-worker ;;
*) fail "the first argument must be controller or worker" ;;
esac
[ "$(id -u)" = 0 ] || fail "run as root"
[ $role = controller ] || [ -n "$dedicated" ] ||
	fail "a worker must be a VM that runs nothing else; confirm with --confirm-dedicated-vm"
[ -x "$here/$binary" ] || fail "run it from the release directory: $here/$binary is missing"

echo "Host"
. /etc/os-release
if [ "${ID:-}" = ubuntu ] && [ "$(printf '%s\n24.04\n' "${VERSION_ID:-0}" | sort -V | head -n1)" = 24.04 ]; then
	ok "Ubuntu $VERSION_ID"
else
	bad "Ubuntu 24.04 or later is required (found ${PRETTY_NAME:-unknown})"
fi
case $(uname -m) in
x86_64) machine=3e ;;
aarch64) machine=b7 ;;
*) machine=none ;;
esac
if [ "$(od -An -tx1 -j18 -N1 "$here/$binary" | tr -d ' ')" = "$machine" ]; then
	ok "the release matches this $(uname -m) host"
else
	bad "the release was built for another architecture than $(uname -m); use make release RELEASE_ARCH=..."
fi
if systemd-detect-virt --vm --quiet; then
	ok "virtual machine ($(systemd-detect-virt --vm))"
else
	bad "not a virtual machine: use a Nitro instance type, not .metal"
fi
[ -f /sys/fs/cgroup/cgroup.controllers ] && ok "cgroup v2" || bad "cgroup v2 is required"
[ "$(timedatectl show -p NTPSynchronized --value 2>/dev/null)" = yes ] && ok "clock synchronized" ||
	warn "the clock is not synchronized yet; controllers must stay within 2 s of the database"

if [ $ec2 = yes ]; then
	echo "EC2"
	token=$(curl -fsS -m 3 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token 2>/dev/null || true)
	if [ -z "$token" ]; then
		bad "the instance metadata service does not answer; is this an EC2 instance? (--not-ec2 skips these checks)"
	else
		code=$(curl -s -m 3 -o /dev/null -w '%{http_code}' http://169.254.169.254/latest/meta-data/instance-id || true)
		[ "$code" = 401 ] && ok "IMDSv2 required" || bad "IMDSv1 is still allowed: set HttpTokens=required on the instance"
		[ -n "$region" ] || region=$(curl -fsS -m 3 -H "X-aws-ec2-metadata-token: $token" http://169.254.169.254/latest/meta-data/placement/region || true)
		warn "the metadata hop limit cannot be read from inside the instance; keep it at 1 (the default)"
	fi
	if systemctl is-active --quiet snap.amazon-ssm-agent.amazon-ssm-agent.service 2>/dev/null ||
		systemctl is-active --quiet amazon-ssm-agent.service 2>/dev/null; then
		ok "SSM agent running"
	else
		warn "the SSM agent is not running; administer through SSH from a bastion instead"
	fi
fi
[ $problems = 0 ] || fail "$problems check(s) failed; nothing was installed"

echo "Prerequisites"
export NEEDRESTART_SUSPEND=1
if [ $role = controller ]; then
	"$here/install.sh" --release "$here" --role controller --prepare-only
else
	"$here/install.sh" --release "$here" --role worker $dedicated --prepare-only
fi

echo "Connectivity"
if [ $role = controller ]; then
	if [ -n "$region" ]; then
		install -d -m 0755 /etc/samuori
		curl -fsSL -o /etc/samuori/rds-ca.pem.tmp "https://truststore.pki.rds.amazonaws.com/$region/$region-bundle.pem" &&
			grep -q 'BEGIN CERTIFICATE' /etc/samuori/rds-ca.pem.tmp &&
			chmod 0644 /etc/samuori/rds-ca.pem.tmp && mv /etc/samuori/rds-ca.pem.tmp /etc/samuori/rds-ca.pem &&
			ok "RDS CA bundle for $region at /etc/samuori/rds-ca.pem" || bad "the RDS CA bundle for $region could not be downloaded"
	else
		warn "no region: place the RDS CA bundle at /etc/samuori/rds-ca.pem yourself (--region)"
	fi
	if [ -n "$database" ]; then
		db_host=${database%:*} db_port=5432
		[ "$db_host" = "$database" ] || db_port=${database##*:}
		pg_isready -q -t 5 -h "$db_host" -p "$db_port" && ok "PostgreSQL answers at $db_host:$db_port" ||
			bad "PostgreSQL does not answer at $db_host:$db_port (security groups, subnet routes)"
	fi
else
	for c in $(echo "$controllers" | tr ',' ' '); do
		c=${c%:8444}
		rc=0
		curl -sk -m 5 -o /dev/null "https://$c:8444/" || rc=$?
		case $rc in
		6) bad "$c does not resolve" ;;
		7 | 28) bad "$c:8444 does not accept connections (sg-controller must allow 8444 from sg-worker)" ;;
		*) ok "$c:8444 reachable" ;;
		esac
	done
	[ -n "$controllers" ] || warn "no --controllers given; port 8444 to the controllers was not checked"
fi
[ $problems = 0 ] || fail "$problems check(s) failed"

echo
echo "Prepared. Install Samuori from this directory with:"
if [ $role = controller ]; then
	echo "  first controller: sudo ./samuori-setup   (answer cluster; or --answers answers.yml)"
	echo "  later ones:       sudo ./samuori-join controller <code from Administration > Servers > Add a controller>"
else
	echo "  sudo ./samuori-join worker <code from Administration > Servers > Add a worker>"
fi
