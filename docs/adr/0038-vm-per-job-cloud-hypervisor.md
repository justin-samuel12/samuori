# 0038. vm-per-job with Cloud Hypervisor

Status: Accepted (2026-10-20). The owner chose Cloud Hypervisor among the providers ADR 0027 allows. Implemented;
**not validated**: no KVM host that can run guests has been available (see "Validation"). Until it is, `vm-per-job`
pools cannot pass their self-test and therefore receive no untrusted builds (ADR 0027, OD-22).

## Context
ADR 0027 makes `vm-per-job` the default profile for untrusted builds: each attempt runs in its own VM, which is
destroyed afterwards. The provider had been deferred until a KVM environment was available (OD-12b).

## Decision
- **Provider `kvm-cloud-hypervisor`** (`internal/adapters/isolation/cloudhypervisor`). Cloud Hypervisor (Apache-2.0
  and BSD-3-Clause) is executed as a separate binary on the worker host; it is never bundled.
- **One VM per attempt.** It boots from a digest-pinned guest image bundle (`tools/vm-image`). The bundle holds the
  kernel, a read-only root filesystem and `manifest.json`; workers pin `samuori-guest@sha256:<manifest digest>`. Each VM
  gets a fresh sparse scratch disk sized to the attempt's disk limit, which holds an overlay over the read-only
  root. Destroying the VM kills the VMM and deletes its directory, scratch disk included. Warm VMs may be booted
  ahead; each still serves one attempt only.
- **No network device.** The VM's only channel is vsock. The guest reaches the host on three kinds of fixed port: the
  attempt's egress proxy session, one relay per configured registry mirror (the host chooses the destination), and
  the attempt's callbacks for secrets and mirror tokens. Egress is strict by construction, with no firewall on the
  worker host needed for it.
- **Inside the guest**, `samuori-worker guest-agent` runs the existing rootless BuildKit path as `rootless-dedicated-vm`:
  a build user confined by nftables to the relay addresses, a systemd scope with the attempt's memory and process
  limits, and one daemon. The guest serves one build or one self-test, never both.
- **Guest output is untrusted.** The host bounds every reply entry, accepts only the expected names, recomputes the
  image digest, and runs the build-secret canary itself, with the values it released, ignoring any canary result
  the guest reports. Runtime-test results come from inside the VM and are recorded as such. Released values are
  masked in logs on the host as well. The host's validation (sensitive content, SBOM, scanners) runs outside the
  VM as for every profile.
- **Limits.** vCPUs and memory bound the VM, with a fixed guest overhead. The scratch disk is the disk quota: a build
  that fills it fails with a resource-limit class. The host enforces the wall clock by destroying the VM.
- **Self-test.** The in-guest probes of ADR 0027 (SEC-ISO-01..05, SEC-ISO-08, SEC-NET-01, SEC-DOS-02/03) run in a
  fresh VM. The host adds three checks: one VM per attempt (SEC-ISO-07); nothing carries from one VM to the next,
  tested with a BuildKit cache mount, which would survive within one daemon (SEC-ISO-09); and the disk quota
  (SEC-DOS-04). A `vm-per-job` pool is verified only when all of them pass (`isolation.SelfTestChecksFor`).

## Validation
On the owner's direction (2026-10-20), nested KVM on the owner's Apple M3 Max (macOS 26, Lima `vz` with nested
virtualization) was tried first and labelled as nested, not bare metal. It cannot run guests. Linux in the L1 VM
runs KVM in nVHE mode, and interrupts are not reliably delivered to L2 guests: timer interrupts stop reaching
vCPUs and boots stall. Cloud Hypervisor v53.0 and QEMU 10.2.1 behave the same, so the fault is in the nested
environment, not the VMM (raw output kept in the maintainer's private records). The host-side
handling of guest output passed tests against a fake guest (SEC-ISO-11). All tests were then removed, and the fake-guest and protocol tests were
restored (ADR 0039 as amended).
The self-test still needs a Linux KVM host where guests run: bare metal, or a cloud instance with nested
virtualization (OD-12b, which needs cost approval).

## Consequences
- A `vm-per-job` worker needs Linux with `/dev/kvm`, Cloud Hypervisor and a guest image built by the operator. The
  image holds Ubuntu packages under their own licenses; Samuori does not distribute it.
- The guest shares its kernel between the agent and the build steps. A step that escapes its rootless container
  inside the VM could forge runtime-test results, but not the image digest, the canary or the host's validation.
- Windows guests and other VMMs (Firecracker, QEMU) remain possible providers behind the same interface.
