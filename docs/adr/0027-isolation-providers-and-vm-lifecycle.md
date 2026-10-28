# 0027. Isolation providers and build VM lifecycle

Status: **Accepted as direction** (2026-10-19, owner decision OD-4). Provider choices are pending spike part 2
(OD-12). No implementation until the profile is verified (ADR 0013).

## Context
OD-4 approved a fresh disposable VM per build as the default for untrusted source, with recycled dedicated VMs per
trust domain as a policy-permitted alternative, and plain rootless BuildKit on shared hosts only for trusted
workloads. The owner required:
- automated VM creation, cleanup, failed-build recovery and resource controls;
- worker capabilities that the scheduler can discover;
- no dependency on a single virtualization implementation;
- an all-in-one install that stays simple.

## Decision

### Isolation provider abstraction
`samuori-worker` talks to an `IsolationProvider` interface, never to a hypervisor directly:

| Operation | Purpose |
|---|---|
| `Capabilities()` | Profile kinds supported, OS/arch, nested-virtualization availability, max CPU/memory/disk, start-up latency class |
| `Prepare(spec)` | Create a build environment from a pinned, digest-verified base image with CPU, memory, disk and time limits |
| `Run(env, plan)` | Start `buildkitd` inside the environment, apply the egress policy (ADR 0025), execute the plan |
| `Collect(env)` | Retrieve logs and commit artifacts to internal storage by digest |
| `Destroy(env)` | Tear down and verify nothing persists (disk, network, processes) |
| `Reconcile()` | Find and destroy environments with no live attempt (crash, cancel, timeout) |

Samuori stays cloud-agnostic. Target environments are on-premises Linux/KVM, Proxmox VE and AWS, with no provider
required by the core product. Candidate providers, each behind the same interface and subject to the integration
gate (`docs/architecture/integrations.md`):
- local KVM via a lightweight VMM (Firecracker or Cloud Hypervisor; both Apache-2.0, license to verify when pinned);
- QEMU/KVM, directly or through libvirt;
- Proxmox VE through its HTTP API (an external, operator-provided service; never bundled; license to verify);
- AWS EC2: one instance per build or warm pools, nested-virtualization-capable instance types, or bare metal;
- Kata Containers, if spike part 2 shows it is viable;
- Lima for macOS development only.

A provider is never assumed to exist. Missing capabilities are reported, not worked around.

### Profiles map to providers

| Profile | Provider requirement |
|---|---|
| `vm-per-job` (default for untrusted builds) | Any provider that can create and destroy an isolated VM per build |
| `rootless-dedicated-vm` (policy-permitted alternative) | A VM dedicated to one trust domain, recycled after N builds or T hours; may be the worker host itself |
| `rootless-host` (trusted workloads only) | Any Linux host with rootless BuildKit |

### Lifecycle automation
- **Creation:** from a versioned, digest-pinned worker image. Optional warm pools hide start-up latency;
  a pooled VM is used once and never returned to the pool.
- **Cleanup:** `Destroy` runs on success, failure, cancellation and timeout. `Reconcile` runs at worker start and
  periodically, and destroys orphans. Cleanup outcomes are recorded on the attempt.
- **Daemon lifecycle (OD-16, OD-18):**
  - Each environment runs exactly one `buildkitd`, serving exactly one build.
  - For secret-bearing builds the daemon starts fresh for that build and is terminated afterwards, together with
    its state directory. This applies inside `rootless-dedicated-vm` too.
  - Destroying the disposable VM (`vm-per-job`) remains the preferred cleanup boundary.
  - Termination reduces the time secrets stay in process memory. It is not cryptographic erasure or secure
    deletion, and Samuori does not claim either.
- **Cleanup triggers:** build completion, failure, cancellation, lease expiry or revocation, worker restart (via
  `Reconcile` at start) and crash recovery (orphaned daemons and environments found by `Reconcile` are destroyed
  before the worker accepts work). Cleanup that fails quarantines the environment or pool.
- **Occupancy:** `Prepare` refuses an environment that already hosts an active build, and `Capabilities()`
  reports free daemon slots, so the scheduler can't over-assign.
- **Failed-build recovery:** infrastructure failures (VM creation, provider errors, lost VM) requeue the run as a
  new attempt with evidence. Application failures do not. Repeated provider failures quarantine the pool and alert
  operators.
- **Resource controls:** limits are set at the VM boundary (vCPU, memory, disk, wall-clock) and, where supported,
  per step inside it (spike part 2). Exceeding a limit is reported as a resource-limit failure, not an application
  error.

### Scheduler discoverability and verification
- Workers advertise their provider capabilities and pool profile. The scheduler routes untrusted builds only to
  pools whose profile is approved and **verified**.
- **Verified** belongs to each worker (amended 2026-10-19, owner decision OD-22): the worker's own self-test (the
  SEC-ISO, SEC-NET and SEC-DOS catalog checks, run in its own environments) passed recently, for its pool's profile
  and its current configuration digest. A failing, outdated or missing self-test removes that worker from
  scheduling for untrusted builds. Pool approval stays a separate, administrator decision; one passing worker never
  verifies a pool.

### All-in-one installs
- On a Linux host with KVM available, all-in-one uses a local VM provider and the `vm-per-job` profile.
- On a host without KVM (for example a cloud VM without nested virtualization), the first-run wizard offers
  `rootless-dedicated-vm`. The administrator confirms the host is dedicated to Samuori and to one trust domain, and
  policy must permit it.
- Preflight explains which profile is available and why, in plain language. Uploaded-source builds stay disabled
  until a verified profile exists.
- Developers never see or configure virtualization.

## Implementation status (2026-10-19)

Provider-independent parts are implemented in `internal/isolation`: the `Provider` interface (the operations above;
`Reconcile` is done by the `Manager` over `Environments`), a lifecycle `Manager` (one build per slot, wall-clock
limit, destroy on every outcome, quarantine when cleanup fails, orphan reconciliation), typed failure classes
(only infrastructure failures requeue), read-only host detection, a provider registry and the eligibility rules.
Pools record immutable approval decisions; approval alone never verifies a pool. Admission refuses uploaded-source
builds until an eligible worker exists. The planned providers (`kvm-firecracker`, `kvm-cloud-hypervisor`,
`kvm-qemu`, `proxmox-ve`, `aws-ec2`) report "planned, needs validation on a KVM host (OD-12b)" and are not
implemented; `samuori-worker` defaults to `--isolation-provider none` and advertises no profiles.

`rootless-dedicated-vm` is implemented by the rootless provider with systemd scope limits and strict egress, and the
worker self-test runs end to end (`docs/architecture/build-execution.md`). Recycling is reported, not automated: the
worker stops accepting builds and the operator replaces the VM.

## Consequences
- More moving parts on worker hosts, hidden behind one interface and first-run preflight.
- Start-up latency is part of build time. Warm pools are the mitigation, measured in spike part 2.
- The provider conformance suite becomes part of the contract tests.
