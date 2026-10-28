# vm-per-job workers

Status: **not validated.** The provider is implemented (ADR 0038). Its acceptance tests never passed and were
removed (ADR 0039), and its self-test has not yet passed on any KVM host. Until they pass on yours, a `vm-per-job` pool fails its self-test and receives no
untrusted builds. Use `rootless-dedicated-vm` workers meanwhile (`all-in-one.md`).

## Requirements
- Linux on amd64 or arm64 with `/dev/kvm`, and the worker user in the `kvm` group. Nested virtualization on Apple
  silicon (Lima `vz`) does **not** work: guests stall (ADR 0038, "Validation").
- Cloud Hypervisor v53.0, checksum-verified (`deploy/dev/lima/provision-kvm.sh` shows the digests).
- A guest image bundle you build yourself, and registry mirrors the host can reach (`egress.md`).

## Build the guest image
On a Linux host with `mmdebstrap`, `e2fsprogs` and `zstd`, build a static `samuori-worker` (`CGO_ENABLED=0`), then run as
root:

```
tools/vm-image/build.sh ./samuori-worker /var/lib/samuori-guest
```

It prints the image reference, `samuori-guest@sha256:…`, which is the digest of `manifest.json`. Keep the bundle
readable by the worker user and writable by root only; the worker refuses files that changed after it verified
them. Rebuild the image to pick up Ubuntu security updates, then restart workers and run the self-test again.

## Run the worker

```
samuori-worker --isolation-provider kvm-cloud-hypervisor --vm-image /var/lib/samuori-guest/manifest.json \
  --mirrors docker.io=10.0.0.5:5000 --slots 2 --warm-vms 1 ...
```

| Flag | Default | Meaning |
|---|---|---|
| `--vm-image` | (required) | Guest image manifest |
| `--vmm` | `cloud-hypervisor` | Cloud Hypervisor binary |
| `--slots` | 1 | Builds at once, each in its own VM |
| `--warm-vms` | 0 | VMs booted ahead for builds with the default limits |
| `--vm-vcpus`, `--vm-memory-gib`, `--vm-disk-gib` | 2, 4, 20 | Default limits; the disk size is the build's disk quota |
| `--vm-max-vcpus`, `--vm-max-memory-gib`, `--vm-max-disk-gib` | 4, 8, 40 | Largest limits a build may request |
| `--mirrors` | none | Registry mirrors as `upstream=host:port`; the guest reaches only these |

The worker reports the guest image reference as its environment image. `--egress unrestricted` is refused: the
guest has no network device.

## Verify
Run the pool self-test from the Workers screen. It boots VMs to run the probes and the host checks, which takes
several minutes. Then confirm that the pool shows **verified**.

The provider's KVM acceptance tests were removed and not restored (ADR 0039 as amended); only its fake-guest and
protocol tests run (`make test-vm`). The self-test above is the only check against real VMs, and it has never passed
on any host.

## Operations
- VM directories live under `--provider-dir`. After a crash, the worker destroys any VMs it finds there on restart.
- Each VM's console log is kept in its directory until the VM is destroyed. Boot failures quote its last lines.
