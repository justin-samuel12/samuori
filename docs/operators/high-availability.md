# High Availability

Status: **implemented; exercised on separate VMs** (installed with the installer, joined with tokens, failover,
partition, quorum loss, worker loss and rolling upgrade, [evidence](#evidence)). Not exercised: physical or cloud
hosts, a cloud load balancer, RDS.

A standalone or all-in-one installation needs none of this; it keeps SQLite and NATS inside the controller.

## Requirements

| Component | Requirement |
|---|---|
| Controllers | Two or more with `role: controller` and `cluster.enabled: true`, the same version and the same configuration |
| Database | PostgreSQL 16–18 (`database.dsn`), reachable from every controller; every controller's clock within 2 s of it (NTP) |
| Messaging | An external NATS cluster with JetStream (`messaging.url`, comma-separated); TLS (`messaging.caFile`) and credentials (`messaging.credentialsFile` or a client certificate) off loopback. Samuori creates its stream with `messaging.replicas` (default 3) |
| Blob storage | Every data class on shared storage: an S3 bucket or an OCI repository (`storage.*`; a registry on a private address needs `--oci-allow-private-network` at `init`). A controller refuses to start if any class would stay in its own data directory |
| Cluster key | One 32-byte key file on every controller (`cluster.keyFile`, mode 0600). It is the secret store key and protects the shared CA material |
| Load balancer | HTTP(S) to every controller's `listen` address, health check `GET /api/v1/system/health`; sticky sessions not required but recommended for the portal during rolling updates (`aws.md#internal-alb`) |
| Workers | `--controller` lists every controller's worker endpoint, comma-separated; each controller's `worker.hostnames` includes the name workers use |
| Audit exports | When purges require exports, `observability.auditExportDir` on shared storage |

## Set up

The installer does all of this (`pilot-guide.md#b-install-a-three-controller-cluster`): `cluster init` on the first
controller and token joins for the rest, with a NATS member on each controller or `--nats-url` for an existing NATS
cluster. The manual steps below are for other layouts.

### Join

Joining a controller takes two steps (ADR 0045):

1. An administrator selects **Create a join code** under Infrastructure > Servers > Add a controller. The portal
   shows one join code (`samuoric_...`, holding the issuing controller's address, its certificate pin and the token) and
   the command to run from the release folder on the new host:
   `sudo ./samuori-join controller <code>`. It takes the host's own name (its hostname, lowercased, with the DNS search domain added when that resolves to this host; otherwise the cloud instance's private DNS name) (`--host NAME` overrides) and the
   release signers from `./allowed_signers` when that file is there (`--release-signers FILE` overrides). It runs
   `sudo ./install.sh --release . [--release-signers ./allowed_signers] --role controller --host HOST join --code
   <code>`, which still works directly; `--url`, `--pin` and `--token-file` still work instead of `--code`.
2. The new host checks the pinned certificate, redeems the token and sends only the SHA-256 of a random secret it
   keeps. Redeeming opens a **join request** and returns no secrets. The host prints its verification code and waits
   up to an hour, polling every 3 seconds.
3. The administrator compares the code under **Join requests** on the Servers page and accepts or rejects it, with an
   optional reason. Only after acceptance does the host, proving its secret, receive the database DSN, cluster key,
   storage settings and certificates for its own name, once. A wrong secret is refused exactly like an unknown
   request; rejected and expired requests are refused. Redemption, acceptance, rejection and collection are audited
   (`controller.join.request`, `controller.join.accept`, `controller.join.reject`, `controller.join`).

Tokens stay single-use, administrator-only, stored hashed, revocable and valid for at most an hour; a request
expires an hour after it opened if it was not accepted and collected. Passing the code as an argument is acceptable
for that reason, although it may reach shell history: a code that leaks before it is used opens at most one request,
which an administrator rejects when the host or verification code is unexpected.

### Join a worker

**Add a worker** on the Servers page (or **Enroll a worker** under Workers & pools) shows one worker join code
(`samuoriwj_...`): this controller's worker endpoints (its non-loopback certificate names and the other live
controllers, on port 8444), the CA pin and a single-use enrollment token valid for an hour. On the worker VM, from the
release folder:

```
sudo ./samuori-join worker <code>
```

It asks to confirm that the VM runs nothing but Samuori builds (or takes `--confirm-dedicated-vm`), checks that a
controller answers with a certificate for the first endpoint's name chaining to the pinned CA, and refuses the code
otherwise, before installing anything. It then runs `install.sh --worker-only --controller <endpoints>` (with
`./allowed_signers` when present) and enrols with the token. The worker joins unassigned; assign it to a pool on the
Servers page. No code is shown when the controller's worker endpoint listens on loopback only; the CA pin and token
are shown either way, for `sudo samuori-enroll-worker <CA pin>` on a host already installed with `--worker-only`
(SEC-WRK-11, TM-115).

1. Create the key once: `samuori-server new-cluster-key /etc/samuori/cluster.key`, and copy it to every controller. To turn a
   standalone controller into the first member, use its `dataDir/keys/secrets.key` as the cluster key, so existing
   secrets stay readable, and move its data to PostgreSQL first (SQLite cannot be shared).
2. Start the first controller. It shares its internal CA through the database; later controllers take that CA, so
   enrolled workers are trusted everywhere and keep their CA pin.
3. Start the others with the same configuration.
4. Point workers at all controllers: `--controller samuori-1.example:8444,samuori-2.example:8444`.

## Resilience

Infrastructure > Servers > **Resilience** shows the tier measured now and the plan (ADR 0046). Each part reports the
failures it survives and what to add:

| Part | Survives | For high availability |
|---|---|---|
| Controllers | a NATS majority: 1 of 3, 2 of 5; none with 1 or 2 | 3 or 5 running controllers |
| Workers | all but one of the online, self-tested workers in each enabled pool | 2 or more in every pool |
| Database | none for SQLite or a single PostgreSQL; 1 when declared replicated | replicated PostgreSQL, such as RDS Multi-AZ, declared in the plan |
| Storage | none for local disk; 1 for S3; 1 for an OCI repository declared replicated | S3 |
| Entry point | none when the portal URL names a controller or loopback; 1 otherwise | a load balancer in front of the controllers |

**Standalone** means no part survives a failure, **Highly available** means every part survives at least one, and
**Partial HA** is anything between. The tier comes from live servers, never from the plan; the plan shows progress
("3 controllers (2 running)") and the tier the plan reaches when complete. Declared properties are labelled
"declared". Changing the plan (`PUT /api/v1/system/topology`, audited) changes no server: add or remove servers with
the join and removal flows.

## Remove a controller

An administrator removes a controller host under Infrastructure > Servers > Controllers > **Remove**,
with a reason and the host name typed to confirm (ADR 0045). The request is refused, with the reason shown, when it
would remove the last controller, while another controller is not running, when the NATS members left would not be a
majority (with a member on each controller: removing one of two is refused; add a controller first), when the
messaging stream's current replicas, as JetStream reports them, would not keep a majority, and while an update
rollout runs. NATS takes a removed member back only five minutes after its removal, so a controller that was removed
and joined again does not count until then, and removing another controller is refused in the meantime. Then:

1. The controller on that host stops contending for leadership and gives up the leader lease at once; another
   controller takes it with a higher epoch, so the departing one can no longer commit singleton work.
2. From the removal on, the departing controller's health answers 503 ("being removed"), so a load balancer takes
   it out of service; it waits 30 seconds (longer than an ALB's two failed checks 10 s apart) before wiping, and
   requests already in flight finish. Once that time has passed and another controller leads, it writes
   `/var/lib/samuori-update/wipe`; `samuori-wipe.path` runs
   `samuori-uninstall --from-samuori` as root, which deletes everything Samuori installed, the NATS member and its data included.
3. The leader revokes the host: it deletes its membership rows and records its API certificate pins, so a controller
   still holding that identity is refused at start (`samuori-server` exits) and stops counting as alive. Request and
   revocation are audited (`controller.remove`, `controller.revoke`).
4. Every remaining controller with a NATS member writes `/var/lib/samuori-update/nats-forget`; `samuori-nats-forget.path`
   runs `samuori-server cluster forget` as root, which drops the host from that member's routes and from the messaging
   URLs in `server.yml`, reloads NATS (`SIGHUP`), and removes the host from the JetStream meta group through the
   system account (`$JS.API.SERVER.REMOVE`). A failure is logged and retried. The stream keeps its replica count;
   with fewer members than replicas it is degraded until a controller joins. The route sync then keeps the host out
   of every member's routes, so its member is refused even if it was not wiped ([NATS routes](#nats-routes)).

A host that does not answer within two minutes, or was not running, is still removed and revoked but shown as **not
confirmed wiped**: run `sudo samuori-uninstall` on it or terminate the instance, and only then consider changing the
database password, because an unwiped host still holds the database credentials and the cluster key. Coming back
means installing again and joining; the new identity an accepted join issues admits the host again.

**NATS system account.** Each NATS member on a controller has a system account (`SYS`, user `samuori-system`) for the
removal step. Its password is generated on that host by the installer, kept in
`/etc/samuori/nats/system-account.password` (root, 0600) and never logged; the member reads only its bcrypt hash from
`system-account.conf`. Controllers keep connecting with their client certificate alone, as the default user of the
global account, so the stream and its data stay where they are. Installs and updates add the account to existing
members (`samuori-server cluster nats-system`, idempotent) and restart `samuori-nats`, which installs and updates already do.
The password is per host rather than per cluster, because only the local helper uses it.

### NATS routes

Every NATS member's routes list exactly the cluster's members, and route TLS uses
`verify_cert_and_check_known_urls`: a member accepts an incoming route only from a peer whose certificate names a host
in its own routes. A removed host that was not wiped keeps a certificate the cluster CA signed, but no member lists it,
so its routes are refused (OD-27, decided 2026-10-23).

- **Plan.** Every controller with a NATS member computes, from the database, the members to route to: controllers
  that recorded their NATS name, plus hosts whose join was accepted (so they can connect when they start), without
  controllers in the **removed** state. Verification is planned only while every running controller has recorded its
  name.
- **Apply.** When the plan changes or a controller starts or stops, then again 5, 10, 20, 40, 80 and 160 seconds
  later, every ten minutes after that, and every minute while a listed member is unconnected or verification waits,
  the controller writes `/var/lib/samuori-update/nats-routes`;
  `samuori-nats-routes.path` runs `samuori-server cluster routes` as root. It brings the member's routes to the plan: removed
  hosts always go; another route the plan does not name goes only while every running controller has recorded its
  name and the member is not connected to that host (an abandoned join, a host pruned after seven days without a
  heartbeat). It refuses a result that would leave the member with no peer,
  writes `nats.conf` atomically, reloads NATS (`SIGHUP`) and confirms through the system account that the reload took
  (the member's configuration load time moves on). A reload that does not take puts the previous file back; a failure
  is logged and retried after 30 seconds. Running it again with the same plan changes nothing. It and
  `cluster forget` hold one lock while they rewrite `nats.conf`.
- **Verification on.** The helper turns verification on only when every peer the member is connected to is in the new
  routes and every member is named by DNS name; otherwise it fills the routes and says what it waits for. This is how
  existing installs converge: an update leaves the seed routes and no verification, the controller fills the routes,
  then turns verification on.
- **Joining.** The existing members list a host as soon as its join is accepted. The new member starts with the routes
  its bundle had and no verification; NATS retries its routes until the members list it, and the controller logs a
  warning while listed members stay unconnected. Its own route sync then fills its routes and turns verification on.
- **Stray routes.** NATS gossip can make a member open a route to a peer it does not list, and a member does not check
  routes it opened itself. If a route to a removed host survives the reload, the helper reports it and the controller
  restarts that host's member, holding a cluster-wide lease so only one member restarts at a time. A restart starts
  the follow-up checks again. Because those checks follow every join, removal and controller start, a stray route
  opened while a joining member still has verification off is found within seconds; the ten-minute check is the
  safety net for anything later.

## Convert between a single server and a cluster

`samuori-server topology convert` moves an installation between a single server (SQLite, blobs on local disk) and the
first controller of a cluster (PostgreSQL, blobs on S3 or an OCI repository), keeping its data, users, history and
images (ADR 0046). It is a maintenance-window operation, not a live migration: Samuori is stopped throughout, and the
command runs as root from the release folder of the version the host already runs.

What it does, in order:

1. **Checks** (all of them, also with `--dry-run`): the host's role; Samuori stopped (no controller heartbeat in the
   source database); the source at this release's schema version (it is never migrated); the target database empty,
   either never migrated or migrated to the same version; the target storage reachable; for a cluster, the
   PostgreSQL version (16–18) and that the database belongs to no cluster; for a single server, exactly one controller
   registered and it is this host, no SQLite database in the data directory yet, and enough free disk for the blobs.
   Settings in `server.yml` the new configuration would not carry are listed; the real run refuses until
   `--allow-dropped-settings` is given. It then prints the row count of every table and the blob count and bytes.
2. **Database:** migrates a fresh target, copies every table in foreign-key order in one transaction, and before
   committing reads every table back from both sides in byte order of the key and compares row counts and checksums
   of every value. A difference rolls the copy back and leaves the target empty. `cluster_material` is not copied:
   each side keeps the CA material in its data directory, and the first controller shares it again when it starts.
3. **Blobs:** every blob is copied and read back, comparing its SHA-256 digest and size. A local blob that does not
   match its name, or a remote object with other content under the same digest, stops the conversion.
4. **Audit:** a `topology.convert` event with the counts is written to the target database.
5. **Configuration:** `/etc/samuori` is copied to `/var/lib/samuori-convert/<time>/etc-samuori` (root only), then the release's
   own installer rewrites the host (below). If the installer fails, the copy is put back, the old services are
   started again, and the command says so.

The source database and source blobs are only read. A conversion that stops after the database copy leaves the
target database filled: drop and recreate it before trying again.

### Single server to cluster

1. Create an empty PostgreSQL 16–18 database owned by the user Samuori signs in as, and an S3 bucket (or OCI repository)
   that every controller can reach. Place the database's CA bundle on the host (default `/etc/samuori/rds-ca.pem`).
2. Check, without writing anything:

   ```
   sudo systemctl stop samuori-server
   cd <release folder>
   sudo ./samuori-server topology convert --to cluster --confirm-dedicated-vm --dry-run --host ctrl1.example \
     --database-host db.example --database-name samuori --database-user samuori --database-ca-file /etc/samuori/rds-ca.pem \
     --s3-bucket samuori-blobs --s3-region eu-west-1
   ```

   The database password is asked on the terminal, or read from `--database-password-file` (mode 0600); it is never
   taken from the command line or printed. Without `--s3-*` or `--oci-repository`, the single server's own
   `storage.s3` or `storage.oci` settings are used.
3. Run the same command without `--dry-run`. The host keeps its worker, which still builds uploaded source there, so
   `--confirm-dedicated-vm` is required (ADR 0027). After the copies, it runs `install.sh --role controller --host
   <host> --confirm-dedicated-vm --keep-local-worker init`, which:
   - uses `--key-file /var/lib/samuori/keys/secrets.key`, so the single server's secret store key becomes the cluster
     key and its stored secrets stay readable;
   - installs the worker parts the all-in-one install installs (BuildKit, RootlessKit, `samuori-worker`, the network,
     firewall and mirror units) and removes `samuori-server.service` and the local registry;
   - then, as `cluster init` does, installs NATS and the controller services, creates the cluster CA, and writes
     `server.yml` (with `localWorker.dir: /var/lib/samuori-local`), `/etc/samuori/env` and the certificates;
   - starts `samuori-nats` and `samuori-controller`.

   The portal is then `https://<host>:8443` (or `--external-url`). The messaging check reports degraded until a
   second controller joins. If the installer fails, the conversion also restores the removed units.
4. The local worker stays enrolled and keeps connecting on `127.0.0.1:8444`; `localhost` and `127.0.0.1` stay in
   `worker.hostnames`. `--keep-local-worker` is kept in `/etc/samuori/install-args`, so every later upgrade and
   self-update reinstalls the controller and the worker together. A rollout updates the worker in its controller's
   step, with a self-test (`upgrades.md#self-update`). Removing this controller drains its worker when the removal
   is requested and revokes it before the controller's rows go; the wipe deletes both (OD-28). Images in the old
   local registry (`127.0.0.1:5001`) stay on disk but are no longer served; publish to a registry every controller
   reaches.
5. Add controllers under **Infrastructure > Servers** ([Join](#join)). The SQLite file and local blobs are no longer
   used; remove them once the cluster is backed up.

### Cluster to single server

1. Remove every other controller under **Infrastructure > Servers** ([Remove a controller](#remove-a-controller)).
   The conversion refuses while any other controller is registered and names them.
2. On the remaining controller:

   ```
   sudo systemctl stop samuori-controller
   cd <release folder>
   sudo ./samuori-server topology convert --to single-server --confirm-dedicated-vm --dry-run
   sudo ./samuori-server topology convert --to single-server --confirm-dedicated-vm
   ```

   The single server builds uploaded source on this host again, so the host must be a VM that runs nothing else
   (ADR 0027); the installer refuses a host that is not a VM. The database DSN and password come from
   `/etc/samuori/env` (`--env-file`).
3. The command writes `/var/lib/samuori/samuori.db`, copies every blob from the shared storage into `/var/lib/samuori/blobs`,
   pulls the latest internal CA from the database, writes the cluster key as `/var/lib/samuori/keys/secrets.key`,
   disables the controller and NATS services, removes the cluster key, database environment and certificates from
   `/etc/samuori`, and runs `install.sh --confirm-dedicated-vm --worker-listen 0.0.0.0:8444 --worker-hostnames
   localhost,127.0.0.1,<host>`, the all-in-one install `samuori-setup` uses. Separate workers stay enrolled and reconnect
   to `<host>:8444`; a local worker enrols in the pool `local` as on any single server.
4. The copy of the old configuration in `/var/lib/samuori-convert` holds the database password: delete it once the single
   server is backed up. The PostgreSQL database and the shared storage are no longer used by this host.

## On Kubernetes (optional)

`deploy/helm/samuori` runs clustered controllers on Kubernetes; standalone and all-in-one installations never need it.
Workers stay on dedicated Linux VMs: build isolation needs a host of its own, so the chart starts no workers and no
privileged pods.

- Bring your own PostgreSQL, NATS cluster and blob storage; the chart refers to existing Secrets for the database DSN,
  cluster key, NATS credentials and CA, and API TLS certificate, and never holds secret values.
- Build the image from `deploy/compose/Dockerfile`, push it to a registry you control and set `image.repository` and
  `image.digest`; nothing is published for you.
- Pods run as a non-root user with a read-only root file system, no capabilities and the runtime's default seccomp
  profile. An init container copies mounted secrets into memory as files only that user can read, as Samuori's key-file
  checks require.
- The worker Service passes TCP through (mutual TLS); put every worker hostname in `worker.hostnames`.
- `hostUsers: false` (Kubernetes user namespaces) lets the discovery sandbox create its own namespaces; otherwise it
  falls back to seccomp only.

`tools/helm/check.sh` lints the chart, validates the rendered manifests against the Kubernetes 1.34 schemas,
checks that incomplete settings fail and that the rendered configuration passes `samuori-server check-config`.

## What happens when a controller stops

- Its heartbeat stops; 15 s later (at once after a clean stop) it counts as gone. Another controller takes the leader
  lease with a higher epoch and runs the singleton duties (garbage collection, CA retirement, audit export, notification
  routing).
- Its workers reconnect to the next endpoint. Running builds continue: the worker presents its leases, the new
  controller renews them, and the result is accepted under the same fencing generation.
- Uploads it was committing fail as interrupted and must be uploaded again; its discoveries fail as interrupted;
  validations restart; publications it was pushing resume (pushes are by digest); pipeline fetches are taken over.
- Work of controllers that are still running is never touched.

## Evidence

> History: the tests named below ran at earlier commits and were removed (ADR 0039); restored focused tests are
> marked in `docs/developers/security-test-catalog.md`.

**In process** (`internal/builds/ha_integration_test.go`, `make test-integration`): two and three controllers on
PostgreSQL with a three-node JetStream cluster; cross-controller delivery, a crash mid-build, a controller losing the
database mid-build, and concurrent dispatch (CHA-HA-01..03, 07).

**Kubernetes rehearsal (2026-10-20)** (`tools/k3s`, logs): k3s v1.37.1 in one VM, two
controller pods from the Helm chart, PostgreSQL 18, a three-node NATS 2.15.1 cluster with client TLS, blob storage in
a loopback registry beside each controller, and a remote worker on its own VM installed with
`install.sh --worker-only`, reaching the worker Service through the host. Results:

- A real build of the Node.js acceptance app ran on the remote worker. Force-deleting the controller pod that held the
  worker's stream mid-build: the worker moved, and the build succeeded 7 s later with one attempt, generation 1 and a
  passing validation.
- Force-deleting the leader pod: leadership moved within 2 s (epoch 2).
- A network partition cutting the stream-holding pod off from PostgreSQL and NATS mid-build: the pod fenced itself and
  went unready, the worker moved (70 s, mostly its reconnect backoff), the build succeeded with one attempt and
  generation 1, the healthy pod led throughout, and the cut-off pod was ready 11 s after healing.
- A rolling update of the controllers (`maxUnavailable: 0`) and a backup through the API on the `controller-pgtools`
  image, verified offline and restored by a Job into a fresh PostgreSQL with every table checked.

The rehearsal found and fixed: the chart letting the image's all-in-one defaults win, missing PostgreSQL client tools,
controllers that kept their workers after losing the database, assignment messages acknowledged when the database
could not be read, and database calls in heartbeats and stream handling without deadlines.

**Separate VMs, installed with the installer (2026-10-20):** six Lima VMs on one Mac (not separate physical or
cloud hosts). One VM: PostgreSQL 18, versitygw v1.8.0 (S3 over TLS from a private CA), an nginx TCP load balancer.
Three controllers, each with one NATS 2.15.1 member, set up with `install.sh --role controller ... init` on the
first and `... join` with portal tokens on the other two. Two workers with `install.sh --role worker`, given all
three controller endpoints, enrolled through the portal and passed their self-tests. The sample app
(`examples/orders-api`) built and validated through the load balancer. Results, after the fixes below:

| Exercise | Result |
|---|---|
| Join | Controllers 2 and 3 joined with single-use tokens; a used token was refused (403); the stream grew to 3 replicas |
| Power off the controller holding the worker's stream, mid-build | Build succeeded 29 s later, one attempt; the worker moved |
| Power off the leader | Leadership moved in 12 s; a new build succeeded |
| Power off the worker running a build | The other worker ran it under generation 2 and succeeded (132 s, mostly lease expiry); the dead worker's session was dropped |
| Two of three NATS members down | Health 503 (messaging unavailable); healthy again 7 s after a second member booted |
| Partition the stream-holding controller from the database, S3 and its peers | It fenced itself (503); the worker moved after 88 s; the build succeeded with one attempt; healthy 29 s after healing |
| Rolling upgrade of all three controllers during a build | Build succeeded, one attempt; 0 failed health polls through the load balancer (every 0.5 s) |

Found and fixed on the way: a first NATS member that refused clustered JetStream, controllers that crash-looped
without JetStream quorum, preflight checks sharing one deadline, workers and controllers that did not notice a
peer whose host lost power (keepalives; the worker endpoint needed HTTP/2 pings), health reporting messaging ok
without quorum, NATS gossiping routes by IP, installers that left empty files after a power loss, and failed
worker calls whose cause was not logged. Not exercised: publication from the cluster (exercised on all-in-one),
ECR, separate physical or cloud hosts, a cloud load balancer.

## Limits

- A removed controller's certificates stay valid against the cluster CA until they expire. Samuori refuses its identity at
  start and the remaining NATS members refuse its routes ([NATS routes](#nats-routes)), but an unwiped host still holds
  the database credentials and the cluster key: wipe or terminate it.
- Route verification stays off while any member is named by IP address, because its certificate carries no DNS name
  to check; such a cluster keeps the unwiped-host route risk. Name controllers by DNS name.
- Rolling back to a release without the route sync leaves verification on: members on that release do not add a new
  controller to their routes, so add `nats-route://<host>:6222` to their `routes` by hand and reload `samuori-nats`, or
  update again first.
- Controller removal was exercised on three Lima VMs; route verification only by focused tests and the in-process
  NATS spike (`docs/spikes/nats-member-removal.md`), not yet on VMs or AWS.

- Conversion ([above](#convert-between-a-single-server-and-a-cluster)) is exercised by focused tests only (table copy
  SQLite→PostgreSQL→SQLite against PostgreSQL 16, refusals, blob digest checks); the command, its installer runs and
  its rollback have not run on a VM, RDS or S3. It supports a NATS member on the controller only, not `--nats-url`.
  The kept local worker (OD-28) is covered by focused tests of the recorded installer options, the rollout step and
  removal; `install.sh --keep-local-worker` itself has not run on a VM. The removal hook drains and revokes every
  enrolled worker named `local`, which a cluster has only on its converted host.

- Per-address request limits are per controller.
- A notification can be sent twice if a controller stops between sending and recording it.
- A worker of a fenced controller reconnects with its usual backoff (up to a minute between tries); its leases last
  longer, so its builds continue, as the rehearsal showed.
- CA rotation on one controller reaches the others within 10 s; workers acknowledge the new bundle from whichever
  controller they are connected to.
