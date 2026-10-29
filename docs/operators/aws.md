# AWS Deployment Profile

Status: **prepared, not deployed.** Nothing here has run on AWS: no AWS resources were provisioned (no
authorization or cost approval). The topology and the installer it uses were exercised on six local VMs
(`high-availability.md#evidence`). Every AWS-specific item below is unverified until it runs in your account.

AWS is one deployment profile, not a dependency. The same installer and binaries run on any Linux VMs with
PostgreSQL and an S3-compatible bucket or OCI repository; ECR is one registry type among several.

## Topology

| Component | Count | AWS service |
|---|---:|---|
| Controllers, each with one NATS JetStream member | 3 | EC2, one per Availability Zone; no builds run here |
| Workers | 1 to start | EC2, dedicated to Samuori builds (`rootless-dedicated-vm`) |
| Database | 1 | RDS for PostgreSQL 16–18, Multi-AZ |
| Shared blob storage | 1 bucket | S3 |
| Published images | as configured | ECR, configured in the portal |
| Browser and API entry point | 1 | Internal Application Load Balancer |

NATS runs on the controllers by default. To use a NATS cluster you already operate, pass `--nats-url` and its
credentials at `cluster init` ([External NATS](#external-nats)).

`vm-per-job` (Cloud Hypervisor) stays disabled: it is not validated on a KVM host (ADR 0038, OD-12b). Workers use
`rootless-dedicated-vm`, one dedicated instance per worker.

## Sizing (starting points, not measured on AWS)

| Host | vCPU / RAM | Disk | Notes |
|---|---|---|---|
| Controller | 2 / 8 GiB | 30 GiB gp3 | Locally, controllers ran on 2 vCPU / 3 GiB. JetStream stores under `/var/lib/nats` |
| Worker | 4 / 16 GiB | 100 GiB gp3 | BuildKit state, base images and the docker.io mirror cache; size disk to your images |

Architectures: arm64 (Graviton) matches what was exercised locally (arm64 Ubuntu 26.04 VMs, and an arm64 Ubuntu
24.04.5 VM for `aws-prepare.sh` followed by an all-in-one install). amd64 was exercised only under emulation. Both
installers pick the right binaries and checksums by `uname -m`. Use virtualized (Nitro) instance types, not `.metal`:
the worker installer requires a VM (`systemd-detect-virt --vm`).

## Deployment checklist

Tick each item before installing. Names in `<angle brackets>` are yours.

**Account and network**
- [ ] Authorization and budget for the account; a VPC with private subnets in three Availability Zones.
- [ ] Outbound HTTPS from controllers and workers (NAT gateway or proxy) during installation: Ubuntu archives and
  `github.com` release downloads (BuildKit, RootlessKit, registry, Syft, nats-server), all checksum-verified.
  Workers also need it at build time for package registries and `registry-1.docker.io`.
- [ ] VPC endpoints (recommended): S3 gateway; ECR `api` and `dkr`, STS, and KMS and Secrets Manager if used.

**EC2**
- [ ] Ubuntu 24.04 LTS or later AMI (cgroup v2, systemd), arm64 or amd64; the same architecture as the release.
- [ ] 3 controller instances, one per AZ; worker instance(s) in a private subnet.
- [ ] IMDSv2 required on all instances. Controllers keep the default hop limit of 1 (samuori-server runs on the host).
- [ ] Administration path: SSM Session Manager or SSH from a bastion.
- [ ] Time sync on (Amazon Time Sync is the Ubuntu default); controllers must stay within 2 s of the database.

**EBS**
- [ ] gp3, encrypted (default EBS encryption or a KMS key) on every instance.
- [ ] Controller root volume holds `/var/lib/samuori` and `/var/lib/nats`; worker root volume holds
  `/var/lib/samuori-worker` and the mirror cache. Separate data volumes are optional, mounted before install.

**IAM**
- [ ] Controller instance profile with the policy in [IAM](#iam).
- [ ] Workers: no instance profile, or one with only `AmazonSSMManagedInstanceCore` for Session Manager. Builds
  cannot reach the metadata service (`architecture/egress.md`), but the host can, so add nothing else.
- [ ] ECR publish roles (and pull roles if used), trusted by the controller role, with an external ID.

**RDS**
- [ ] PostgreSQL 16–18, Multi-AZ, encrypted, in private subnets; a database `samuori` owned by role `samuori`.
- [ ] `sg-rds` allows 5432 from `sg-controller` only.
- [ ] The RDS CA bundle for your region at `/etc/samuori/rds-ca.pem` on the **first** controller (root-owned, 0644);
  `samuori-setup` downloads it when it is missing:
  `curl -fsSLo /etc/samuori/rds-ca.pem https://truststore.pki.rds.amazonaws.com/<region>/<region>-bundle.pem`. Joining
  controllers receive it in the join bundle and keep it as `/etc/samuori/database-ca`.
- [ ] The password URL-encoded in the DSN if it holds `@ : / ? # %`.
- [ ] From a controller: `psql '<dsn>' -c 'select 1'` succeeds (the installer installs `postgresql-client`).

**S3**
- [ ] One bucket, Block Public Access on, default encryption (SSE-S3 or SSE-KMS; with SSE-KMS the controller role
  also needs `kms:GenerateDataKey` and `kms:Decrypt` on that key).

**TLS and DNS**
- [ ] A private DNS name for each controller, resolvable by every controller and worker (a Route 53 private hosted
  zone, or the EC2 private DNS names). Choose before install: `--host` goes into the controller's certificates.
- [ ] A name for the ALB, such as `samuori.<internal domain>`, as an alias record in the private zone.
- [ ] An ACM certificate for that name on the ALB listener (public ACM with DNS validation, ACM Private CA, or an
  imported certificate). Controllers get their own certificates from the cluster CA; nothing to provide.
- [ ] Browsers that use Samuori trust the ALB certificate's issuer.

**Security groups** (ports from the actual listeners; see [Security groups](#security-groups))
- [ ] `sg-alb`, `sg-controller`, `sg-worker` and `sg-rds` created with the rules in the table.

**Release**
- [ ] `make release RELEASE_ARCH=<arm64|amd64>` and `make release-verify`; copy `dist/samuori-linux-<arch>` to each host.

## Prepare hosts

`aws-prepare.sh`, in the release directory, readies a vanilla Ubuntu 24.04 instance without installing Samuori. Run it
on each host, or once while baking an AMI:

```sh
sudo ./aws-prepare.sh controller [--region <region>] [--database-host <rds-endpoint>]
sudo ./aws-prepare.sh worker --confirm-dedicated-vm [--controllers <c1>,<c2>,<c3>]
```

| Step | Controller | Worker |
|---|:---:|:---:|
| Checks Ubuntu 24.04+, the release architecture, a Nitro VM, cgroup v2 and clock sync | ✓ | ✓ |
| Checks IMDSv2 is required and the SSM agent runs (warns without SSM) | ✓ | ✓ |
| Installs the installer's apt packages and checksum-verified downloads (`install.sh --prepare-only`) | ✓ | ✓ |
| Downloads the RDS CA bundle for the region to `/etc/samuori/rds-ca.pem`; checks PostgreSQL answers | ✓ | |
| Checks every controller accepts connections on 8444 | | ✓ |
| Prints the command to run next (`samuori-setup` on the first controller) | ✓ | ✓ |

It stops before any Samuori binary, user, certificate or service, and calls no AWS API. A failed check installs
nothing. The metadata hop limit cannot be read from inside an instance; keep the default of 1. `--not-ec2` skips the
EC2 checks on a local VM. Verified on a local Ubuntu 24.04.5 VM with `--not-ec2`; the EC2 checks (IMDSv2, SSM, the
region from metadata) have not run on EC2 yet.

## Installation order

1. **Controller 1:** `sudo ./samuori-setup` and answer `cluster` ([samuori-setup](all-in-one.md#install-with-samuori-setup)). It
   takes the DNS name from the hostname (qualified by the search domain; instance metadata only when that does not resolve to the host) and the region from instance metadata, downloads the RDS global CA bundle to `/etc/samuori/rds-ca.pem`
   when it is missing, signs in to RDS, probes the bucket, and then runs `install.sh --role controller ... init`.
   `--answers answers.yml` does the same unattended. The long `init` command
   (`pilot-guide.md#b-install-a-three-controller-cluster`) still works. Until a second controller joins, the first
   reports `degraded` ("waiting for a second controller to join"): the portal and API work, so the rest of the setup
   is done there, but builds wait for a NATS majority.
2. **ALB:** register ctrl1 in the target group ([ALB](#internal-alb)); its `degraded` health answers 200, so the ALB
   routes to it. Register each other controller only after it has joined: a registered host that is not running
   Samuori gets requests whenever the ALB fails open, and the portal then loads only partly.
3. **Join codes:** sign in through the ALB or at `https://<ctrl1>:8443`. Under Infrastructure >
   Servers > Add a controller, select **Create a join code**. It shows one join code and the command.
4. **Controllers 2 and 3:** run `sudo ./samuori-join controller <code>` from the release folder on each, with its own
   code. It takes the host's own name (its hostname, lowercased, with the DNS search domain added when that resolves to this host; otherwise the cloud instance's private DNS name) (`--host` overrides) and
   `./allowed_signers` when present. The code names
   the controller that issued the token, directly on 8443, not the ALB: the pin is that controller's certificate.
   The host prints a verification code and waits; accept its request under **Join requests** once the codes match
   (`high-availability.md#join`). A token works once and expires within an hour; only after acceptance does the host
   receive the database DSN, cluster key, storage settings, the database CA and certificates over that pinned
   connection. Nothing is copied by hand.
5. **Check:** three live controllers under Infrastructure > Servers; every target healthy; health `ok` once the
   stream has three replicas.
6. **Workers:** under Infrastructure > Servers, select **Add a worker** and run `sudo ./samuori-join worker <code>`
   from the release folder on each worker instance. The code names every controller on 8444; the worker checks the
   pinned certificate before installing, then enrols unassigned (`high-availability.md#join-a-worker`,
   `pilot-guide.md` B.5). The two-step `--role worker --controller ...` install with `samuori-enroll-worker` still works.
7. **Registries:** add ECR under Build supply > Registries & destinations ([ECR](#ecr)).
8. **Acceptance:** `pilot-guide.md#e-acceptance-checklist`.

## Network paths

Every connection is TCP over TLS inside the VPC. Controllers never connect to workers.

| From | To | Port | Purpose | Protection |
|---|---|---:|---|---|
| Client networks | ALB | 443 | Browser and API | TLS with the ACM certificate |
| ALB | Controllers | 8443 | Browser and API | HTTPS to the controller's cluster-CA certificate |
| Controller | Controllers | 8443 | Joining a controller | Certificate pin and a single-use token |
| Workers | Every controller | 8444 | Enrollment, leases, dispatch, source, secrets, logs and results | gRPC over mutual TLS |
| Controller | Controllers | 4222 | NATS client connections | Mutual TLS, cluster CA |
| Controller | Controllers | 6222 | NATS cluster routes and JetStream replication | Mutual TLS, cluster CA |
| Controllers | RDS | 5432 | Database | TLS verified against the RDS CA bundle |
| Controllers | S3, ECR, STS | 443 | Storage, publication, role assumption | Instance role |
| Workers | NAT or proxy | 443 | Package registries and Docker Hub during builds | Build egress policy |

With [External NATS](#external-nats), 4222 and 6222 between controllers are not needed; controllers reach the
external cluster instead.

## Security groups

| Group | Direction | Port | Peer | Purpose |
|---|---|---:|---|---|
| `sg-alb` | Inbound | 443 | Client networks (prefix list) | Browser and API |
| `sg-alb` | Outbound | 8443 | `sg-controller` | Targets |
| `sg-controller` | Inbound | 8443 | `sg-alb` | API from the ALB |
| `sg-controller` | Inbound | 8443 | `sg-controller` | Controller join |
| `sg-controller` | Inbound | 8444 | `sg-worker` | Worker gRPC |
| `sg-controller` | Inbound | 4222 | `sg-controller` | NATS client |
| `sg-controller` | Inbound | 6222 | `sg-controller` | NATS cluster |
| `sg-controller` | Outbound | 8443, 4222, 6222 | `sg-controller` | Join and NATS |
| `sg-controller` | Outbound | 5432 | `sg-rds` | Database |
| `sg-controller` | Outbound | 443 | NAT or VPC endpoints | S3, ECR, STS, installation downloads |
| `sg-worker` | Inbound | none | | Administration through SSM needs no port; for SSH, add 22 from the bastion's group |
| `sg-worker` | Outbound | 8444 | `sg-controller` | Worker gRPC |
| `sg-worker` | Outbound | 443 | NAT or proxy | Build egress and installation downloads |
| `sg-rds` | Inbound | 5432 | `sg-controller` | Database |

Reference security groups, not CIDRs, inside the VPC, and do not allow all egress from workers: they build untrusted
source. Monitoring agents may add their own group. Samuori's Prometheus endpoint is off by default; with
`observability.metricsListen` set, allow that port from the scraper's group (a bearer token is required off loopback).

## Internal ALB

Use an Application Load Balancer, not a Network Load Balancer. It carries browser and API traffic only, ends TLS with
an ACM certificate and removes controllers whose health check answers 503; a TCP health check would keep routing to
a controller that is listening but cannot serve. Workers connect to controllers directly on 8444, so no load balancer
carries worker traffic. Use an NLB only when a policy requires one, and then for 443 to 8443 only. Never put 8444 behind a
load balancer that ends TLS: the controller must see the worker's client certificate.


- Listener HTTPS 443 with the ACM certificate. Target group: instance targets, protocol **HTTPS**, port 8443,
  HTTP/1.1. The ALB does not validate target certificates, so the controllers' cluster-CA certificates work.
- Health check: `GET /api/v1/system/health`, protocol HTTPS, port 8443, success code `200`, interval 10 s, healthy
  threshold 2, unhealthy threshold 2. It needs no credentials. A controller answers 200 when `ok` or `degraded`
  and 503 when `unavailable` (database, storage or NATS quorum lost, or the controller fenced itself after a
  partition), so the ALB routes only to controllers that can serve. Locally, a fenced controller dropped out and
  came back 29 s after healing.
- When every target is unhealthy, the ALB routes to all of them (fail open). Keep hosts that are not running Samuori
  out of the target group, and expect the portal to load only partly while a cluster has lost its NATS majority. A
  first controller waiting for a second one is `degraded`, not unavailable, so it stays in service.
- Stickiness on, with a load-balancer-generated cookie for an hour or more. Controllers do not need it (sessions,
  lockouts and leases live in the database), but it keeps one page's requests on one controller: during a rolling
  update a page from the new release could otherwise ask an old controller for scripts it does not have. When the
  controller goes unhealthy (removed, restarting, updating) the ALB moves the browser to a healthy one.
- Deregistration delay 30 s, the time a removed controller drains before it wipes itself.
- Idle timeout 300 s. Build progress uses server-sent events that stay quiet during long steps; the browser
  resumes from the last event if the ALB closes one.
- The ALB carries browser and API traffic only. Workers and joining controllers never use it.
- Behind the ALB, every client's address is an ALB node's address: the per-address sign-in limit (20 per minute
  per controller) is shared by all users. Per-account lockout is unaffected. Forwarded headers are not trusted.

## How workers reach controllers

- Each worker dials every controller's worker endpoint on TCP 8444 directly, by the private DNS names in
  `--controller`. It is gRPC over mutual TLS. Controllers never connect to workers.
- At enrollment, the worker checks the controller's certificate against the CA pin shown in the portal, then
  receives its own client certificate for the single-use enrollment token. All controllers serve the same cluster
  CA, so one enrollment is trusted by all three.
- Each controller's worker certificate carries the names in `worker.hostnames`: its own `--host`, the others
  added at join, and any `--worker-hostnames` given at init. Workers verify against the first name in
  `--controller`, which every controller's certificate carries.
- Workers hold no AWS credentials and no database or NATS access. A worker that disappears loses its lease; its
  build moves to another worker (exercised locally in 88–132 s).

## IAM

Controllers authenticate with their instance role (`workload-identity`). Workers get **no** AWS credentials.

Controller instance role (scope resources to your names):
- S3: `s3:ListBucket` on the bucket; `s3:GetObject`, `s3:PutObject` and `s3:DeleteObject` on its objects.
- ECR publishing: either the ECR actions below on the instance role itself (profile identity **This controller's
  IAM role**), or `sts:AssumeRole` on the publish roles you configure in the portal (cross-account with an
  external ID). Those roles carry only the push actions on their repositories, and pull roles only
  `ecr:GetAuthorizationToken`, `ecr:BatchGetImage` and `ecr:GetDownloadUrlForLayer` (`registry-auth.md`).
- Secrets Manager (if used as a secret provider): `secretsmanager:GetSecretValue` on the secret prefix.
- KMS signing (if used): `kms:Sign` and `kms:GetPublicKey` on the signing key.

IAM Roles Anywhere is **not implemented**. Long-lived access keys are a fallback (`access-key` with a credentials
file), never on workers.

## RDS

### Create the instance, role and database

1. **Instance:** RDS for PostgreSQL 16, 17 or 18, in the controllers' VPC, private subnets, **Public access: No**.
   Multi-AZ is optional; Samuori retries a read or transaction once after a failover drops its connections
   (`database.md#lost-connections`). Storage encryption on; automated backups on.
2. **Parameter group:** a custom one with `rds.force_ssl = 1`, so no connection without TLS is accepted.
3. **Security group:** inbound 5432 only from the controllers' security group ([Security groups](#security-groups)).
   Workers never reach the database.
4. **Role and database:** connect once as the master user from a controller host (`sudo apt-get install -y
   postgresql-client` if `psql` is missing) and create a login role that owns an empty database:
   ```
   curl -so /tmp/rds-ca.pem https://truststore.pki.rds.amazonaws.com/<region>/<region>-bundle.pem
   psql "host=<endpoint> user=<master> dbname=postgres sslmode=verify-full sslrootcert=/tmp/rds-ca.pem"
   CREATE ROLE samuori LOGIN PASSWORD '<password>';
   CREATE DATABASE samuori OWNER samuori;
   REVOKE ALL ON DATABASE samuori FROM PUBLIC;
   ```
   The owner may create tables in its database's `public` schema (PostgreSQL 15 and later give that schema to the
   database owner), which is all the migrations need; `samuori` needs no superuser or `rds_superuser` membership. A
   separate application role (`samuori_app`, `database.migrationDsn`) is optional (`database-migration.md`).
5. **Password:** `samuori-setup` asks for it with no echo and URL-escapes it in the DSN. If you write the DSN by hand,
   percent-encode any character other than letters, digits and `-._~`, or use a long letters-and-digits password.
   Change it later from Infrastructure > Database in the portal (`database.md#rotate-the-password`), not only in RDS.
6. **Check** from a controller host before running `samuori-setup`:
   `psql "host=<endpoint> user=samuori dbname=samuori sslmode=verify-full sslrootcert=/tmp/rds-ca.pem" -c 'select version()'`.
   `samuori-setup` repeats the check: it signs in, requires PostgreSQL 16–18, and refuses a database that already holds a
   cluster.

To start again on the same instance, drop and recreate only this database (`DROP DATABASE samuori WITH (FORCE);` then
`CREATE DATABASE samuori OWNER samuori;`) and empty the bucket prefix ([S3](#s3)); other databases on the instance are not
touched.

### Connection

`postgres://samuori:PASSWORD@<endpoint>:5432/samuori?sslmode=verify-full&sslrootcert=/etc/samuori/rds-ca.pem`. `cluster init`
signs in, reads the schema and refuses a database that already belongs to a cluster; joining controllers receive
the DSN over the pinned join and check it before writing anything. The DSN is stored only in `/etc/samuori/env`
(mode 0600). Back up with RDS snapshots and `pg_dump` (`backup-restore.md`).

## S3

Pass `--s3-bucket` and `--s3-region` at `cluster init`; leave the endpoint empty for AWS. Joining controllers
inherit the settings. Preflight writes, reads and deletes a probe object on every start.

## ECR

Configured in the portal after installation, under Build supply > Registries & destinations: registry profiles per account and
region, the role to assume and the external ID. The three verification levels are separate. The adapter is tested
against an emulator. The portal's connection test checks identity, account, region and permissions. A real push
of a test image is the only end-to-end proof, and has not been run.

### Use the controller's instance role

1. Attach the IAM policy below to the controllers' instance profile (every controller, since any of them may
   publish).
2. Under Build supply > Registries & destinations, **Add registry** with kind **Amazon ECR**, the account ID and the region. An
   endpoint such as `123456789012.dkr.ecr.eu-west-1.amazonaws.com` added as kind OCI cannot use IAM roles; the
   dialog says so.
3. **Add credentials** for that registry, purpose Push, AWS identity **This controller's IAM role (EC2 instance
   profile or the default AWS credentials)**. No secret is stored. Optionally limit repository scopes.
4. **Test** the credentials, then add destinations.

The controller uses the AWS SDK's default credential chain, which on EC2 is the instance profile through IMDS. For
**Assume a role**, the instance role needs only `sts:AssumeRole` on that role, and the role carries the actions
below.

Actions Samuori calls (from `internal/adapters/registry/ecr` and the OCI pushes made with the ECR token):

| Purpose | Actions | Resource |
|---|---|---|
| Registry token (every push and test) | `ecr:GetAuthorizationToken` | `*` (it cannot be scoped) |
| Connection test, repository list | `ecr:DescribeRepositories` | the repositories |
| Push an image, signatures and attestations | `ecr:BatchCheckLayerAvailability`, `ecr:InitiateLayerUpload`, `ecr:UploadLayerPart`, `ecr:CompleteLayerUpload`, `ecr:PutImage` | the destination repositories |
| Verify the pushed digest, tag checks, promotion source reads | `ecr:BatchGetImage`, `ecr:GetDownloadUrlForLayer` | the destination repositories |
| Create repositories (admin action, D03) | `ecr:CreateRepository` | the repositories; give it a separate profile if you can |

`sts:GetCallerIdentity`, used by the test to report the identity and account, needs no permission. This list comes
from the code and the ECR documentation's mapping of registry API calls to actions; it has not been checked
against a live account (INT-REG-03).

## External NATS

Optional. At `cluster init`, add `--nats-url tls://<n1>:4222,tls://<n2>:4222,tls://<n3>:4222`,
`--nats-ca-file <ca.pem>` and either `--nats-credentials-file <samuori.creds>` or `--nats-cert-file` with
`--nats-key-file`. The controller then runs no NATS member, and joining controllers receive the same NATS
settings and credentials over the pinned join; ports 4222 and 6222 between controllers are not needed. The NATS
account needs JetStream with room for one stream (`--replicas`, default 3). Focused test only; not yet exercised
against a running external cluster.
