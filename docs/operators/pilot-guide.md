# Pilot Guide

How to install, reach, test and operate Samuori for a hands-on evaluation. What is verified and what is not is in
`../release-readiness.md`; AWS specifics are in `aws.md`.

## A. Install one host (all-in-one)

On a dedicated Ubuntu 24.04+ or Debian 13 VM with systemd and cgroup v2 (arm64 or amd64):

```
make release RELEASE_ARCH=arm64                     # on your workstation; amd64 for x86-64 hosts
scp -r dist/samuori-linux-arm64 host:                   # then on the host:
sudo ./samuori-linux-arm64/install.sh --release ./samuori-linux-arm64 --confirm-dedicated-vm
```

It installs samuori-server, the local worker with rootless BuildKit, strict egress, the docker.io mirror and a local
registry on `127.0.0.1:5001`. Running it again with a newer release upgrades in place.

## B. Install a three-controller cluster

Prerequisites: PostgreSQL 16–18 reachable from every controller, an S3 bucket (or an OCI repository) for shared
blobs, DNS names for the controllers, and the release directory on each host. Controllers run no builds.

1. **First controller** (`ctrl1`):
   ```
   sudo ./install.sh --release . --role controller --host ctrl1.example init \
     --database 'postgres://samuori:PASSWORD@db.example:5432/samuori?sslmode=verify-full&sslrootcert=/etc/samuori/rds-ca.pem' \
     --s3-bucket samuori-blobs --s3-region us-east-1 \
     --external-url https://samuori.example --organization example \
     --admin-username admin --admin-password-file /root/admin-password
   ```
   It creates the cluster key, the cluster CA and its NATS member, and checks the database. Until a second controller
   joins it reports `degraded` ("waiting for a second controller to join"): the portal works, builds wait for a NATS
   majority.
2. **Join code:** sign in, open Infrastructure > Servers > Add a controller, and select **Create a join code**.
   Copy the command it shows.
3. **Second and third controllers**, from the release folder:
   ```
   sudo ./samuori-join controller samuoric_...
   ```
   It takes the host's own name (its hostname, lowercased, with the DNS search domain added when that resolves to this host; otherwise the cloud instance's private DNS name) (add `--host ctrl2.example` to choose it) and `./allowed_signers` when
   present. The long form `sudo ./install.sh --release . --release-signers ./allowed_signers --role controller --host
   ctrl2.example join --code samuoric_...` does the same. The host prints a verification code and waits. Accept its request under **Join requests** once the codes match;
   only then does it receive its configuration and certificates over a connection pinned to `ctrl1`
   (`high-availability.md#join`). Each token works once and expires within an hour. Repeat with a new token for
   `ctrl3`.
   To use an existing NATS cluster instead of a member on each controller, add `--nats-url`, `--nats-ca-file` and
   its credentials at init (`aws.md#external-nats`); joining controllers inherit them.
4. **Check:** Infrastructure > Servers lists three live controllers, and `GET /api/v1/system/health` on each says
   `ok` once the stream has three replicas.
5. **Workers**, on separate dedicated VMs. In Infrastructure > Workers & pools, create a `rootless-dedicated-vm`
   pool (creating it approves it). Under Infrastructure > Servers, select **Add a worker** and run the command it
   shows from the release folder on the worker VM:
   ```
   sudo ./samuori-join worker samuoriwj_...
   ```
   It confirms the VM is dedicated, checks the controller's pinned certificate, installs the worker pointed at every
   controller and enrols it (`high-availability.md#join-a-worker`). The worker appears unassigned; choose "Assign to
   pool". It then runs its self-test; uploaded source builds only after it passes. The two-step form still works:
   `sudo ./install.sh --release . --confirm-dedicated-vm --role worker --controller
   ctrl1.example:8444,ctrl2.example:8444,ctrl3.example:8444`, then `sudo samuori-enroll-worker <CA pin>` with the token
   pasted.

To remove a controller or a worker, select **Remove** for it under Infrastructure > Servers: it hands over
leadership, wipes Samuori from its host and is revoked (`high-availability.md#remove-a-controller`). Keep at least three
NATS members for a tolerant majority.

## D. Access

- UI and API: the load balancer address, or `https://<controller>:8443` (all-in-one: `http://127.0.0.1:8080`).
- First administrator: the one named at `cluster init`. All-in-one prints a one-time setup code with
  `sudo samuori-setup-code`.
- Health: `curl -sk https://<controller>:8443/api/v1/system/health`. `ok` means ready; `degraded` names a
  non-critical issue; `unavailable` (HTTP 503) means the load balancer should not route to it.
- Preflight, on a controller: `sudo samuori-server preflight --config /etc/samuori/server.yml`, with
  `SAMUORI_DATABASE` from `/etc/samuori/env`. It checks the database sign-in and schema, NATS, and a write, read and
  delete on storage.
- The CLI trusts the load balancer's certificate, or the cluster CA at `/etc/samuori/cluster-ca.crt` when talking to a
  controller directly.

## E. Acceptance checklist

Run it through the load balancer as a real user would. Each item names what to check; anything else is a failure
to record.

**Browser interface**
- [ ] The UI loads over HTTPS at the external URL with no certificate warning; sign in as the administrator.
- [ ] Infrastructure > Servers lists every controller as live; health is `ok` on each.
- [ ] Under Teams and Users, create a team and a developer with the developer role. Sign in as the developer:
  administration pages are refused, not merely hidden.

**Worker enrollment**
- [ ] Under Infrastructure > Workers & pools, create a `rootless-dedicated-vm` pool; it shows as enabled, approved by you.
- [ ] Join a worker with `samuori-join worker` (B.5). It appears online and unassigned; assign it to the pool, then its
      self-test passes.
- [ ] Reusing the same enrollment token on another host is refused.

**Image build**
- [ ] As the developer, create a project and upload `examples/orders-api`. Discovery reports Node.js and Express
  without running the project's code.
- [ ] The Form and YAML views show the same spec.
- [ ] Start a build; progress and logs stream. The run shows validation, sensitive-content findings and an SBOM.
- [ ] Cancel a second build: it ends cancelled with a reason, not failed or succeeded.
- [ ] Rebuild from history succeeds.

**Registry configuration**
- [ ] Under Registries, add a destination: ECR with the publish role and external ID (`aws.md#ecr`), or the
  local registry `127.0.0.1:5001` on all-in-one. Allow the project on it.
- [ ] The connection test passes, and a deliberately wrong role or region fails with a clear reason.

**Publication**
- [ ] Publish the built image. The publication records the digest.
- [ ] The digest in the registry matches (`crane digest <ref>`, or `aws ecr describe-images`).
- [ ] Publishing from a project the destination does not allow is refused.

**Resilience (cluster only, optional)**
- [ ] Stop one controller during a build: the build finishes and the ALB marks that target unhealthy.

On all-in-one, `make aio-clean test-acceptance` runs the browser and build items automatically.

## F. Readiness

See `../release-readiness.md`. In short: the all-in-one browser journey and the three-controller cluster were
exercised on local VMs. `vm-per-job`, AWS (RDS, S3, ECR, ALB) and live integrations are not verified.

## G. Operations

| Task | Command |
|---|---|
| Restart | `sudo systemctl restart samuori-controller samuori-nats` (controller), `samuori-server` (all-in-one), `samuori-worker` (worker) |
| Logs | `journalctl -u samuori-controller -u samuori-nats -u samuori-worker -f` (JSON lines) |
| Upgrade | Re-run `install.sh` with the new release on one host at a time; keep two controllers up |
| Add a worker | Install with `--role worker`, then enroll with a token (B.5) |
| A worker shows quarantined | It could not clean up an environment. Check `journalctl -u samuori-worker`; restart it; it destroys leftovers on start |
| A controller is not ready | `preflight` names the failing dependency; messaging needs a majority of NATS members |
| Back up and restore | `backup-restore.md` |
| Reset a disposable pilot | Stop the services, drop and recreate the database, empty the bucket, delete `/etc/samuori`, `/var/lib/samuori` and `/var/lib/nats` on controllers and `/var/lib/samuori-worker/*` on workers, then install again from A or B |

More detail: `runbook.md`.
