# vm-per-job isolation

Status: implemented, **not validated** on a KVM host (ADR 0038). This page describes the protocol between the
worker host and its build VMs; ADR 0038 records the decisions.

## Layout

| Part | Where |
|---|---|
| Host provider | `internal/adapters/isolation/cloudhypervisor` (`kvm-cloud-hypervisor`) |
| Guest agent and protocol | `internal/vmguest` (`samuori-worker guest-agent`) |
| vsock transport | `internal/vsock` (AF_VSOCK in the guest, Cloud Hypervisor hybrid vsock on the host) |
| Guest image | `tools/vm-image/build.sh` |

## Boot
The VMM boots the bundle's kernel with the read-only root (`/dev/vda`) and the attempt's scratch disk
(`/dev/vdb`). `overlay-init` formats the scratch disk, mounts an overlay over the root and starts systemd; the
agent unit starts the agent on vsock port 1024. The host polls `GET /v1/ready` until the build user's systemd
manager is up, or the boot timeout passes.

## Ports

| Port | Listener | Purpose |
|---|---|---|
| 1024 | guest agent | `GET /v1/ready`, `POST /v1/run`, `POST /v1/selftest` |
| 1025 | host | `POST /v1/secret`, `POST /v1/mirror-token` for the attempt the VM is assigned to; refused otherwise |
| 1026 | host | The egress proxy; guest steps reach it at `10.88.0.1:3128` through a relay |
| 1100+n | host | Relay to mirror n; guest steps reach it at `10.88.0.1:5000+n` |

`10.88.0.1` exists only on a dummy interface inside the guest. The guest's nftables rules allow the build user to
reach those relay addresses and nothing else.

## Run stream
`POST /v1/run` carries a tar stream: `request.json` (attempt, limits, proxy session URL, mirrors), `plan.json`, the
source under `source/` and an optional verified cache under `cache/`. No secret values travel in it; the guest asks
port 1025 for each one while the build runs. The reply is a tar stream: `result.json` first (class, detail, logs,
digests, checks), then at most `image.tar` and `cache.tar`. The host's limits on the reply are in ADR 0038.

## Failure classes
`result.json` carries `resource-limit`, `timeout`, `infrastructure`, `unsupported` or `build`. Only infrastructure
failures requeue (ADR 0027). A dead VMM, a broken exchange or a digest mismatch is an infrastructure failure; an
expired wall clock destroys the VM and is a timeout.
