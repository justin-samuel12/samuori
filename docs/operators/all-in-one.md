# Running the all-in-one controller

Status: **Current for WP23.** Covers installing on one dedicated Linux VM, natively or with Compose, through to
publishing an image to a local OCI registry.

## Install with samuori-setup

`sudo ./samuori-setup`, in the release folder, is the first-node setup for a single server or the first controller of a
cluster (ADR 0045). It asks, in order, and checks each answer before the next question:

| Question | Default | Check |
|---|---|---|
| Deployment: `cluster` or `single-server` | `single-server` | a single server must be a VM that runs nothing but Samuori (asked) |
| Planned controllers and build workers (cluster) | `3`, `2` | 1–7 controllers; an even number is warned about (it survives no more failures than one fewer) and needs a `yes` |
| This host's DNS name | the hostname, lowercased and with the DNS search domain added, when that resolves to this host; else EC2 `local-hostname`; else the hostname | resolves |
| Portal URL | `https://<DNS name>` | a scheme and host only; `https`, or `http` on loopback |
| Database (cluster): host, port, name, user, password, CA bundle | `5432`, `samuori`, `samuori`, `/etc/samuori/rds-ca.pem` | signs in, PostgreSQL 16–18, holds no cluster yet; a missing CA bundle is downloaded (the AWS RDS global bundle) when the host is on AWS |
| Database replicated across zones, such as RDS Multi-AZ (cluster) | `no` | none: Samuori cannot measure it, so it is shown as declared |
| Database (single server) | built-in SQLite | none |
| S3: bucket, region, prefix (optional for a single server) | region from instance metadata | writes, reads back and deletes a probe object under `samuori-setup-probe/` |
| Organization; administrator username, email and password | `admin` | password policy (12 characters or more, not containing the username) |
| Release public key (`allowed_signers`) | `./allowed_signers` when present; blank disables updates from the portal | holds a key for `release@samuori.invalid` |

The summary shows the plan and the resilience it reaches: for each part (controllers, workers, database, storage,
entry point) what it survives and what to add for high availability (ADR 0046). The plan is written to
`/etc/samuori/topology-plan.yml` and imported once when the server first starts; afterwards it is changed under
Infrastructure > Servers > Resilience.

A failed check shows the error and asks again. Passwords are typed with echo off and never read from the command
line; any password that would appear in a check error or the installer's output is masked. After a summary and a
`yes`, it runs `install.sh` (a single server: `--confirm-dedicated-vm` with the portal URL, S3 and the first
administrator; a cluster: `--role controller --host <name> init` with the database DSN passed in the environment),
waits until the server has created the administrator, and deletes the password file it was created from.

For automation, `sudo ./samuori-setup --answers answers.yml` reads the same answers from a file, runs the same checks,
stops at the first failure and installs without asking. The file may hold passwords, so it must be a regular file
owned by root with mode `0600`; anything else is refused. Fields left out take the defaults above.

```yaml
deployment: cluster          # or single-server, with dedicatedVm: true
controllers: 3               # cluster only; default 3
workers: 2                   # cluster only; default 2
host: ctrl1.example.com
portalUrl: https://samuori.example.com
database: {host: samuori.abc.eu-west-1.rds.amazonaws.com, user: samuori, password: "...", caFile: /etc/samuori/rds-ca.pem, replicated: true}
s3: {bucket: samuori-blobs, region: eu-west-1, prefix: prod}
organization: Example
admin: {username: admin, email: admin@example.com, password: "..."}
releaseSigners: ./allowed_signers
```

Status: covered by focused tests (`internal/setupcli`); not yet run end to end on a VM, against RDS or on EC2. The
`install.sh` commands below remain for upgrades and for setups `samuori-setup` does not cover (Compose, a remote worker,
an external NATS cluster). Further controllers and remote workers join with `samuori-join`
([Add a remote worker](#add-a-remote-worker), `high-availability.md#join`). A single server becomes a cluster's
first controller, and back, with `samuori-server topology convert`
([Convert between a single server and a cluster](high-availability.md#convert-between-a-single-server-and-a-cluster)).

## Install on a dedicated Linux VM

Requirements: a Debian or Ubuntu VM with systemd and cgroup v2 that runs nothing but Samuori (the `rootless-dedicated-vm`
profile, ADR 0027), internet access for docker.io and package registries, and root. For the Compose shape, Docker
Engine with the Compose plugin. Hardware hosts need `vm-per-job`, which is not available yet (OD-12b).

```sh
make release RELEASE_ARCH=amd64        # or arm64; produces dist/samuori-linux-<arch>/
# copy that directory to the VM, then on the VM, either:
sudo ./install.sh --release . --confirm-dedicated-vm                        # native (systemd)
sudo ./install.sh --release . --confirm-dedicated-vm --server-in-compose    # Compose
```

Both shapes install the same host parts and differ only in where the controller and the local registry run (ADR
0032). Running the installer again with a newer release upgrades in place: it stops the worker, reapplies the
firewall, restarts the services (or `docker compose up -d --build --wait`), then starts the worker again.

| Part | How |
|---|---|
| Tools | Checksum-pinned BuildKit v0.34.0, RootlessKit v3.3.0, Distribution v3.1.2 and Syft v1.54.1; OS packages `uidmap`, `slirp4netns`, `nftables` |
| Users | `samuori` (controller), `samuori-worker`, `samuori-build` (runs build daemons; subordinate IDs, lingering), `samuori-registry`; shared group `samuori-builds` |
| `samuori-network` | A dummy interface `samuori0` with `10.231.0.1` (`--bridge-address`), where the egress proxy and the mirror listen, so neither is exposed on the LAN |
| `samuori-egress-firewall` | The `samuori-worker egress-rules` nftables table: the build user may reach only the proxy and the mirror. The worker unit requires it, so the worker stops if the firewall does |
| `samuori-mirror` | docker.io pull-through mirror on `10.231.0.1:5000`; with `--railpack` also `samuori-mirror-ghcr`, a ghcr.io pull-through mirror on `10.231.0.1:5002` for the Railpack images (`providers.md`); each `--registry-mirror <host>` adds `samuori-mirror@<host>`, a pull-through mirror for that registry from `10.231.0.1:5003` up (below) |
| Controller | Native: `samuori-server.service` on `127.0.0.1:8080` (`--listen`), config `/etc/samuori/server.yml`, data `/var/lib/samuori`, hardened unit. Compose: the `samuori-server` container, read-only, all capabilities dropped, `no-new-privileges`, host networking so its listeners stay on loopback, health check `samuori-server healthcheck`; no Docker socket and no privileged mode |
| Local registry | OCI registry for publication on `127.0.0.1:5001`: `samuori-local-registry.service` (skip with `--no-local-registry`) or the Compose `registry` service with a health check |
| `samuori-worker` | `rootless-dedicated-vm` worker named `local`; `samuori-worker.path` starts it when the controller offers an enrollment |
| AppArmor | Per-binary `userns` profiles for `rootlesskit` and `samuori-server` (sandboxed scans); the host-wide restriction stays on |

**Base images from other registries.** Under strict egress a build pulls only through the mirrors. When an
administrator approves a base-image request for a registry other than docker.io (or ghcr.io with `--railpack`),
rerun the installer on each worker host with `--registry-mirror <host>`, once per registry, for example
`sudo ./install.sh --release . --confirm-dedicated-vm --registry-mirror mcr.microsoft.com`. Each mirror keeps its
port across reruns (`/etc/samuori/registry-mirrors`), the option is kept for self-updates, and leaving a host out on a
later run removes its mirror. The mirrors pull anonymously; private registries use image sources
(`../architecture/registry-auth.md`). Details: `../architecture/egress.md#mirrors-for-other-registries`.

**The host worker needs no manual steps.** Once setup has created the organization, the controller keeps a pool named
`local` (profile `rootless-dedicated-vm`, disabled) and writes a single-use enrollment token, valid for one hour,
and its CA pin into `/var/lib/samuori-local` (mode 0700, readable only by the controller and root). The worker unit's
root pre-start step copies them to the worker, which enrolls over the mutually authenticated worker endpoint
(`127.0.0.1:8444`) and deletes its copy; the controller removes the token once the worker is enrolled. A revoked
local worker is never re-enrolled automatically, and the pool is never enabled automatically: pools an administrator
creates are approved by that act, but the controller created this one.

Then, in the browser:

1. Open the UI and complete setup with the code from `sudo samuori-setup-code` (not needed after `samuori-setup`, which
   creates the organization and administrator).
2. **Infrastructure → Workers & pools**: the `local` worker appears within seconds. Choose **Enable pool** on
   the `local` pool. Enabling starts the worker's own self-test (about a minute); uploaded source builds once it passes.
   Workers are retested after configuration changes and every 20 hours.
3. **Build supply → Registries & destinations**: add `127.0.0.1:5001` with **Plain HTTP** and **private network**,
   credentials of type None, and a destination for your projects.

### Add a remote worker

A controller installed with `--worker-listen 0.0.0.0:8444 --worker-hostnames samuori.example.com` (add
`--no-local-worker` for one that runs no builds itself) accepts remote build VMs. Under **Infrastructure → Servers**,
select **Add a worker**, then on the build VM, from the release folder:

```
sudo ./samuori-join worker samuoriwj_...
```

It confirms the VM is dedicated (or takes `--confirm-dedicated-vm`), checks the controller's pinned certificate
before installing anything, runs `install.sh --worker-only --controller samuori.example.com:8444` and enrols with the
code's single-use token (`high-availability.md#join-a-worker`). The worker appears unassigned; assign it to a pool.
A controller whose worker endpoint listens on loopback only shows no code. The two-step form, `install.sh
--worker-only` and then `sudo samuori-enroll-worker <CA pin>`, still works; see [`worker-identity.md`](worker-identity.md),
which also covers renewal, revocation and CA rotation.

Reaching the UI from another machine needs TLS (`tls.certFile`, `tls.keyFile` in `/etc/samuori/server.yml`, or
`SAMUORI_TLS_*` in Compose, and a non-loopback `listen`), or an SSH tunnel. The mirror uses plain HTTP on the local `samuori0`
address only. In Compose, whether the scan sandbox gets user namespaces inside the container (Docker's default
seccomp profile may refuse them) is not verified yet; when they are refused it falls back to seccomp only and records
which protections applied.

## Evidence

`make aio-clean` / `make aio-compose` build a release, copy it into a fresh Lima VM (`deploy/dev/lima/samuori-clean.yml`,
`samuori-compose.yml`) and run the installer; `make test-acceptance` / `make test-acceptance-compose` then run
`ui/e2e/acceptance/vertical-slice.spec.ts` against it. `deploy/dev/lima/reset-clean.sh [vm]` returns a VM to a
just-installed state.

The journey covers first-run setup, a developer upload of a Node.js Express API with a real lockfile, a build that is
recorded as Blocked before any worker is verified, automatic enrollment of the local worker, enabling its pool and the
worker's self-test, the local registry destination, **Build again**, a successful build with runtime test and
validation verdict `passed`, publication, and a check against the registry that the tag points to the published
digest.

**WP23 (2026-10-19)**, macOS host, Lima VMs on Apple Virtualization, Ubuntu 26.04 arm64, in the order it happened:

1. **Clean-VM installs.** Both VMs were deleted and recreated, then installed from `make release`: natively with
   `install.sh`, and with `install.sh --server-in-compose` plus `docker compose up -d --build`. Both installs
   succeeded and passed steps 1–4 of the journey. Step 5 then failed on test assertions only: the Images table shows
   the verdict in lowercase, and the test compared the stored archive digest with the destination manifest digest.
   The product had built, validated and published the image in both shapes.
2. **Assertion fixes and data resets.** After those test-only fixes, the reset scripts cleared the application data
   (database, worker identity, local registry) on the same freshly installed VMs, without reinstalling. The full
   journey was rerun: all five steps passed natively and via Compose.

**OD-22 and OD-23 (2026-10-19)**, same host and images:

1. **Clean-VM installs.** Both VMs were deleted and recreated, then installed with one command each (the Compose shape
   now runs Compose itself). Both installs succeeded; the Compose controller reported healthy. The local worker
   enrolled by itself about a second after setup in both shapes.
2. **Test fixes and data resets.** Step 3 first failed twice on the test itself: a wait matched the words "has not
   passed recently" in the eligibility text, and the page was read before the controller had created the `local`
   pool. After those test-only fixes and two application-data resets on the same VMs, all five steps passed natively
   and via Compose.
3. **Upgrade and recovery.** Re-running the installer on both VMs (upgrade in place) and then rebooting both left every
   service active, both Compose services healthy, the firewall loaded, and the local worker connected and still
   verified for its unchanged configuration.

**WP24 regression (2026-10-19)**: both VMs recreated and installed from scratch. Step 3 failed on the test only: the
pools table had gained a Location column, which broke a text pattern. After fixing the pattern and one
application-data reset on the same VMs, all five steps passed natively and via Compose.

**Debian 13 (2026-10-20)**: a clean Debian 13 (trixie) VM, kernel 6.12 cloud build with no AppArmor profiles loaded
and unprivileged user namespaces allowed (`deploy/dev/lima/samuori-debian.yml`, `make aio-debian`,
`make test-acceptance-debian`). The installer finished on the first run and all five acceptance steps passed,
including the local worker's self-test under strict egress and cgroup limits, a real build, validation and
publication.

**Linux amd64, emulated (2026-10-20)**: a clean Ubuntu 26.04 amd64 VM under QEMU emulation on the Apple silicon host
(`deploy/dev/lima/samuori-amd64.yml`, Lima 2.2.0 with its x86_64 guest agent installed side by side, no hardware
acceleration), installed from `make release RELEASE_ARCH=amd64` (a commit in the pre-publication history). All five steps of the journey
passed, including a real rootless BuildKit build, validation and publication (7.2 minutes). The development suites
also ran on an emulated amd64 VM (`samuori-dev-amd64.yml`) at an earlier commit, before the tests were removed (ADR 0039): integration 654 and BuildKit/Buildah 224
tests passed; the unit run passed except one test whose fixed 10-second wait was too short under emulation (it passed
in isolation and in the integration run)
(raw output kept in the maintainer's private records). Emulation is not validation on native amd64 hardware.

Not covered: native amd64 hardware, Debian and Ubuntu releases other than Debian 13 and Ubuntu 26.04.

## Build from source and start (development)

```sh
make build ui-install
(cd ui && npm run build)
bin/samuori-server --data-dir /var/lib/samuori --ui-dir ui/dist
```

Plain HTTP is accepted only on a loopback listen address. For any other address, set `--tls-cert-file` and
`--tls-key-file`; session cookies are then marked `Secure`.

## Settings

Precedence: flags, then `SAMUORI_*` environment variables, then the YAML file named by `--config` / `SAMUORI_CONFIG`, then
defaults. The startup log lists the source of every setting without values that could be sensitive.

| Key | Flag / environment | Default | Purpose |
|---|---|---|---|
| `listen` | `--listen` / `SAMUORI_LISTEN` | `127.0.0.1:8080` | HTTP(S) listen address |
| `storage.s3.bucket`, `.region`, `.prefix`, `.endpoint`, `.pathStyle` | `--s3-bucket`, `--s3-region`, … / `SAMUORI_S3_*` | empty (local only) | Object storage for blobs; the data directory becomes a cache ([storage backends](../architecture/registry-and-artifacts.md#storage-backends)) |
| `storage.s3.authMethod`, `.roleArn`, `.externalId`, `.credentialsFile` | `--s3-auth-method`, … | `workload-identity` | AWS identity for the bucket; an access-key file must be mode 0600 |
| `storage.cacheEvictAfter` | `--cache-evict-after` / `SAMUORI_CACHE_EVICT_AFTER` | `24h` | With object storage, remove local copies older than this |
| `storage.oci.repository`, `.credentialsFile`, `.plainHttp`, `.allowPrivateNetwork` | `--oci-storage-repository`, … / `SAMUORI_OCI_STORAGE_*` | empty | OCI registry repository for blobs; the credentials file holds `USERNAME:PASSWORD` (mode 0600); plain HTTP only to loopback |
| `buildCache.ttl`, `buildCache.maxScopeBytes` | `--build-cache-ttl`, `--build-cache-max-scope-bytes` | `168h`, 10 GiB | Shared build caches older or larger than this are not imported and are removed ([build cache](../architecture/build-cache.md#shared-caches-d26)) |
| `storage.routes.source`, `.image`, `.report` | `--storage-route-source`, … / `SAMUORI_STORAGE_ROUTE_*` | default backend | `local`, `s3` or `oci` for each data class ([storage backends](../architecture/registry-and-artifacts.md#storage-backends)) |
| `imageSources.sharedMirrors` | `--shared-mirrors` / `SAMUORI_SHARED_MIRRORS` | `false` | Let one authorizing mirror serve several teams (OD-14) |
| `externalUrl` | `--external-url` / `SAMUORI_EXTERNAL_URL` | empty | URL browsers use, such as `https://samuori.example`; needed for OIDC sign-in ([identity](../architecture/identity.md)) and absolute links in notifications |
| `smtp.host`, `.port`, `.from`, `.username`, `.passwordFile` | `--smtp-host`, … / `SAMUORI_SMTP_*` | empty (email off), port 587 | SMTP relay for email notifications; STARTTLS required off loopback; the password file must be mode 0600 ([notifications](../architecture/notifications.md)) |
| `dataDir` | `--data-dir` / `SAMUORI_DATA_DIR` | `/var/lib/samuori` | SQLite database and bootstrap token |
| `database.dsn` | `--database` / `SAMUORI_DATABASE` | SQLite in `dataDir` | `sqlite://…` or `postgres://…` |
| `ui.dir` | `--ui-dir` / `SAMUORI_UI_DIR` | empty (API only) | Built web UI directory (`ui/dist`) |
| `tls.certFile`, `tls.keyFile` | `--tls-cert-file`, `--tls-key-file` | empty | PEM certificate and key |
| `bootstrap.*` | see below | empty | Configuration-as-code first administrator |
| `localWorker.dir` | `--local-worker-dir` / `SAMUORI_LOCAL_WORKER_DIR` | empty (off) | All-in-one, or a cluster controller that keeps its host's worker (ADR 0046, OD-28): 0700 directory where the controller hands that worker its enrollment token |
| `localWorker.profile` | `--local-worker-profile` / `SAMUORI_LOCAL_WORKER_PROFILE` | `rootless-dedicated-vm` | Isolation profile of the `local` pool |
| `observability.metricsListen` | `--metrics-listen` / `SAMUORI_METRICS_LISTEN` | empty (off) | Separate Prometheus metrics listener |
| `observability.metricsTokenFile` | `--metrics-token-file` / `SAMUORI_METRICS_TOKEN_FILE` | empty | Bearer token for scrapes (0600 file); required off loopback |
| `observability.tracing.endpoint` | `--tracing-endpoint` / `SAMUORI_TRACING_ENDPOINT` | empty (off) | OTLP/HTTP traces URL |
| `observability.tracing.sampleRatio` | `--tracing-sample-ratio` / `SAMUORI_TRACING_SAMPLE_RATIO` | `1` | Fraction of new traces sampled |
| `observability.auditExportDir` | `--audit-export-dir` / `SAMUORI_AUDIT_EXPORT_DIR` | `audit-export` in `dataDir` | Scheduled audit exports ([observability](../architecture/observability.md)) |
| `federation.listen`, `.hostnames`, `.allowPrivatePeers` | `--federation-listen`, … / `SAMUORI_FEDERATION_*` | empty (off), `false` | Listener for federation peers ([federation](federation.md)) |
| `backup.dir`, `.recipientsFile`, `.interval`, `.keep`, `.pgDump`, `.pgRestore` | `--backup-dir`, … / `SAMUORI_BACKUP_*` | empty (off), `0`, `7` | Encrypted backups ([backup and restore](backup-restore.md)) |
| `cluster.enabled`, `cluster.keyFile` | `--cluster`, `--cluster-key-file` / `SAMUORI_CLUSTER*` | `false`, empty | Run as one of several controllers ([high availability](high-availability.md)) |
| `messaging.url`, `.credentialsFile`, `.caFile`, `.certFile`, `.keyFile`, `.replicas` | `--messaging-url`, … / `SAMUORI_MESSAGING_*` | empty (embedded NATS), 3 replicas | External NATS cluster; TLS and credentials required off loopback |

## First run

Until setup completes, the server writes a one-time setup code to `<dataDir>/bootstrap-token` (mode 0600) and logs
the file path, never the code. Open the web UI, enter the code, and create the organization and first
administrator. The code expires after 24 hours (restart the server to issue a new one) and is deleted once used.
Setup cannot be repeated.

For unattended installs, set all three of `bootstrap.organizationName`, `bootstrap.adminUsername` and
`bootstrap.adminPasswordFile` (and optionally `bootstrap.adminEmail`). The password file must be readable only by its owner (`chmod 600`). The values are
used only while no organization exists. The password must meet the password policy. `dev.weakPasswords`
(default `false`) exempts local accounts from the policy for local development; the server refuses it unless it is
an all-in-one server on a loopback address. Never set it on an installation other people use.

## Workers

The controller serves workers on `worker.listen` (default `127.0.0.1:8444`) with certificates from its internal CA
in `<dataDir>/pki` (keep `ca.key` private and backed up). To enroll a worker:

1. Create a pool (`POST /api/v1/worker-pools`), then a token for it (`POST /api/v1/enrollment-tokens`). The token
   and the CA pin are shown once.
2. Write the token to a file readable only by the worker's user (`chmod 600`).
3. Run `samuori-worker --controller <host:port> --ca-pin <pin> --enrollment-token-file <file> --state-dir <dir>`.
   The worker deletes the token file after use.

Enrolled workers are listed as not eligible for untrusted builds until they are assigned to an enabled pool
and their self-test has passed (`POST /api/v1/worker-pools/{poolId}/self-tests`, or **Run self-test**). The only
provider that can pass it in this release is `rootless-dedicated-vm`, on a Linux VM dedicated to the worker
(`docs/architecture/build-execution.md`). `vm-per-job` needs a provider validated on a KVM host. Without a verified
pool, every build of uploaded source is recorded as `blocked` with the reason. The same steps are available in the UI
under Infrastructure → Workers & pools. Infrastructure → Build queue, Build engines, Storage (with on-demand garbage
collection) and Messaging show live state from the controller.

## Source uploads

Uploaded source is stored under `<dataDir>/blobs` (content-addressed, verified on every read) and staged under
`<dataDir>/uploads` until committed. Limits and rules: `docs/architecture/sources.md`.

## Sign-in protection

Passwords use argon2id. After 5 failed attempts an account is locked for 15 minutes, and each client address is
limited to 20 sign-in attempts per minute. Failures and lockouts are kept in the database, so controllers of a cluster
share them; the per-address limit applies per controller. Sessions end after 30 minutes idle or 12 hours in total by default; administrators change both on the Identity page
(docs/architecture/identity.md#sessions). API tokens are
shown once, stored only as hashes, expire within a year, and are read-only unless created with write access.
