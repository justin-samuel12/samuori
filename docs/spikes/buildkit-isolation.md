# Spike: BuildKit Isolation and Build Egress

Date: 2026-10-19. Status: **Part 1 complete.** Part 2 is **deferred, awaiting an authorized testing environment**
(OD-12b); see the end of this document. Raw output is kept in the maintainer's private records. Scripts:
`tools/spikes/buildkit-isolation/`.

## Question
Which isolation profiles can Samuori support for untrusted builds, what are their limitations, and which should be the
default (OD-4)? Can default-deny egress coexist with normal dependency downloads (ADR 0025)?

## Environment

| Item | Value |
|---|---|
| Host | Apple M3 Max, macOS 26.7.1, personal device (owner-confirmed) |
| VM | Lima 2.2.0, `vz`, nested virtualization on, 6 vCPU / 12 GiB, **no host mounts** |
| Guest | Ubuntu 26.04 LTS, kernel 7.0.0-28-generic, cgroup v2 |
| BuildKit | v0.34.0 (sha256 `0619ca3e…82c9`) |
| RootlessKit | v3.3.0 (checksum verified against upstream SHA256SUMS) |
| gVisor | release-20261012.0 (checksum verified against upstream SHA256SUMS) |
| Kata Containers | 4.2.0 static arm64 (sha256 `5dd4e9f2…376b`; upstream publishes no checksum file) |
| Proxy / mirror (spike only) | tinyproxy (Ubuntu package); CNCF Distribution `registry:3` as a pull-through cache |
| Fixtures | `probe/Dockerfile`, `limits/Dockerfile`; fictitious secret value |

Only configuration properties were probed. No exploit code was run.

## Profiles tested

| Profile | What it is | Result |
|---|---|---|
| P1 `rootless` | Rootless `buildkitd` under RootlessKit (slirp4netns, host loopback disabled) on a dedicated VM | **Works.** Core properties validated |
| P1+E | P1 with strict egress: uid-scoped nftables, proxy allowlist, pull-through mirror | **Works.** Egress design validated |
| P1+L | P1 under a systemd unit with MemoryMax/TasksMax | **Partly works** (see F10, F11) |
| P2 `compose-rootless` | `moby/buildkit:v0.34.0-rootless` in Docker with upstream's unconfined options | Works only after weakening the host (F14) |
| P5 `kata` | Rootful `buildkitd` + containerd + Kata runtime (QEMU micro-VM per step) | **Not evaluated:** failed in the triple-nested environment (F15) |
| P6 `gvisor` | Rootless `buildkitd` with `runsc` as the OCI runtime | **Not usable** without custom integration (F13) |
| P4 `vm-per-job` | P1 inside a fresh VM per job | Components validated; VM lifecycle not yet measured on a real hypervisor (F17) |

## Results matrix

`✔` = holds, `✘` = does not hold, `–` = not tested.

| Property | P1 | P1+E | P2 | P6 |
|---|---|---|---|---|
| Step root maps to an unprivileged host UID | ✔ | ✔ | ✔ | ✔ (user-space kernel) |
| Seccomp filter on build steps | ✔ (mode 2) | ✔ | ✔ | n/a (gVisor intercepts syscalls) |
| Host kernel exposed to steps | ✘ (7.0 host kernel) | ✘ | ✘ | ✔ hidden (`4.19.0-gvisor`) |
| No Docker/containerd/BuildKit sockets in steps | ✔ | ✔ | ✔ | ✔ |
| BuildKit telemetry socket absent from steps | ✘ (`/dev/otel-grpc.sock`) | ✘ | ✘ | ✘ |
| Direct internet blocked | ✘ | ✔ | ✘ | – |
| DNS blocked | ✘ | ✔ | ✘ | – |
| Worker-host services blocked | ✘ (host IP reachable) | ✔ | ✘ | – |
| Host loopback blocked | ✔ | ✔ | ✔ | – |
| Allowlisted registry via proxy | – | ✔ (npm) | – | – |
| Non-allowlisted host via proxy denied | ✔ | ✔ | ✔ | – |
| Secret absent from image blobs and BuildKit state | ✔ | – | – | – |
| Daemon-level memory limit contains the build | – | – | – | – |
| (P1+L) memory | ✔ contained, but daemon killed (F10) | | | |
| (P1+L) process count | ✔ step fails cleanly, daemon survives (F11) | | | |

The metadata endpoint showed "blocked" everywhere, but Lima has no metadata service, so that result is inconclusive
(F16).

## Findings

- **F1** Ubuntu 23.10+ blocks unprivileged user namespaces by default (`kernel.apparmor_restrict_unprivileged_userns=1`),
  so RootlessKit fails. A per-binary AppArmor profile granting `userns` to `/usr/local/bin/rootlesskit` fixes it
  without weakening the host. The sysctl stays at 1.
- **F2** In P1 build steps, root is mapped to the build user; other IDs map to subordinate IDs. Seccomp is on,
  `NoNewPrivs` is 0, and the default capability set applies inside the user namespace. The host kernel is shared.
- **F3** BuildKit mounts `/dev/otel-grpc.sock` (its tracing socket) into every step. It is a channel from
  untrusted code to the daemon. It should be disabled or proven harmless (part 2).
- **F4** With default rootless networking, steps reach the internet, DNS and any service listening on the worker's
  own IP. `--disable-host-loopback` blocks only loopback.
- **F5** Strict egress works. Firewall rules matching the build user's UID (the slirp4netns process) allow only the
  proxy and mirror ports. All other traffic is dropped, including DNS and worker-host services. The proxy allowed
  `registry.npmjs.org` and denied other hosts with 403.
- **F6** With the proxy alone, base-image pulls failed: registry requests reached the proxy, but the registry
  token request was not seen there and was dropped by the firewall. The root cause was not investigated. Pulling
  through a Samuori-controlled registry mirror worked and is the better design anyway (approved sources only).
- **F7** Docker-published ports are rewritten (DNAT) before the filter rule sees them. Rules must match the
  original destination (`ct original`). This is an implementation note for the worker firewall.
- **F8** BusyBox `wget` ignores `https_proxy`. With strict egress such tools fail closed, as intended. Recipes
  must use proxy-aware tools or set tool-specific proxy settings.
- **F9** A secret mounted with `--mount=type=secret` was absent from every exported image blob and from BuildKit's
  on-disk state.
- **F10** Without cgroup delegation, build steps share the daemon's cgroup. With `MemoryMax=1G`, the oversized step
  was killed, but so was `buildkitd`, even with `OOMPolicy=continue`. The worker sees a dropped connection. Until
  per-step limits exist, run one build per daemon and report daemon death as "resource limit exceeded", citing the
  unit result (`oom-kill`).
- **F11** `TasksMax=256` stopped a 600-process fork loop; the step failed cleanly and the daemon survived.
- **F12** `buildctl --output type=oci,dest=…` wrote a world-writable (0666) file. The worker must control output
  file modes.
- **F13** gVisor runs steps under its user-space kernel (`4.19.0-gvisor`). But BuildKit passes runc-only flags
  (`--keep`), and every step is reported as failed when the runtime exits. Making it usable needs a maintained
  runc-compatible shim or BuildKit support, so it is not a supported profile now.
- **F14** The Compose profile needs `seccomp=unconfined` and `apparmor=unconfined` for the daemon container. On
  Ubuntu 23.10+ it also needs the host-wide sysctl `kernel.apparmor_restrict_unprivileged_userns=0`, because the
  restriction applies even to unconfined containers. Steps still get BuildKit's seccomp filter. Weakening the host
  is acceptable only when the host is itself a dedicated, disposable VM.
- **F15** Kata: QEMU micro-VMs started, but the shim failed to reach the guest agent ("Connection refused").
  `/dev/vhost-vsock` was present. The likely cause is the triple-nested setup (macOS → Lima → QEMU). Kata also
  needs a rootful containerd and a rootful `buildkitd` on the worker. It needs a KVM host to evaluate fairly.
- **F16** The metadata-endpoint checks are inconclusive here. On cloud workers they must be retested; strict
  egress drops link-local traffic regardless.
- **F17** The first boot of a Lima `vz` VM, including the image download, took about 48 s. That is an upper bound.
  Per-job VM latency needs measuring on the target hypervisor (Firecracker, Cloud Hypervisor or a pre-warmed pool).

Not tested in part 1: disk quotas, CPU limits, build timeouts, cache isolation between trust domains,
multi-architecture emulation, and image signing. The design gives each trust domain its own daemon and cache (ADR 0013), so cross-domain cache tests belong to part 2.

## Supported profiles and limitations

| Profile | Isolation boundary | Supported for | Main limitations |
|---|---|---|---|
| `vm-per-job` | Hypervisor; VM discarded after each build | **Untrusted builds (recommended default)** | Start-up latency; needs a hypervisor on worker hosts; orchestration not built yet |
| `rootless-dedicated-vm` | Rootless containers in a VM dedicated to one trust domain, recycled on a schedule | Untrusted builds where per-job VMs are impractical | Shared kernel across jobs in the same trust domain; a compromised job can persist until the VM is recycled |
| `rootless-host` | Rootless containers directly on a shared Linux host | Trusted internal builds only | Shared host kernel; host compromise affects everything on the host |
| `compose-rootless` | As `rootless-dedicated-vm`, with a weaker daemon profile | Evaluation and single-user installs inside a disposable VM | Requires host-wide user-namespace weakening (F14) |
| `kata` | Micro-VM per build step | Candidate pending part 2 | Rootful daemons on the worker host; nested-virtualization constraints |
| `gvisor` | User-space kernel | Not supported | No working BuildKit integration (F13) |

Every profile requires strict egress (ADR 0025), daemon-level resource limits, and one build per daemon until
per-step limits are proven.

## OD-4 outcome

The owner approved (2026-10-19):
- `vm-per-job` as the default for untrusted builds;
- `rootless-dedicated-vm` where policy permits;
- `rootless-host` for explicitly trusted workloads only.

Uploaded-source builds stay disabled until a pool's profile is approved and verified. Providers and lifecycle
automation are in ADR 0027.

## Part 2: nested KVM on the owner's Mac (2026-10-20)

On the owner's direction, part 2 was first attempted on personal equipment at no cost: Lima `vz` with nested
virtualization on the M3 Max (`deploy/dev/lima/samuori-kvm.yml`), labelled as nested and not bare metal. `/dev/kvm` is
present and the L1 kernel initializes KVM ("Hyp nVHE mode": Apple's virtual EL2 offers no VHE). L2 guests start but
cannot run: with Cloud Hypervisor v53.0 and with QEMU 10.2.1, timer and device interrupts stop reaching L2 vCPUs
(RCU stalls, soft lockups, a single vCPU stalling before its root filesystem mounts). The `vm-per-job` provider
(ADR 0038) was therefore implemented and tested against a fake guest, and its KVM tests remain unrun
(raw output kept in the maintainer's private records). This also explains F15: Kata's agent was unreachable in
the same environment. QEMU was installed in the `samuori-kvm` VM for this diagnosis only. Part 2 still needs one of the
hosts below, or a personal Linux machine with KVM.

## Part 2 plan (deferred)

Status: deferred, awaiting an authorized testing environment (OD-12b, 2026-10-19). Nothing has been provisioned.
The scope below stays valid for whichever authorized environment is used. Its target platforms are on-premises
Linux/KVM, Proxmox VE and AWS (ADR 0027). The cost estimate is kept for reference only and was not approved.

### Scope
1. Per-build VM start-up and teardown time (cold and warm pool), with at least two VMMs (Firecracker or Cloud
   Hypervisor, and QEMU).
2. Kata Containers feasibility on real KVM: agent connectivity, start-up time, rootful-daemon implications.
3. CPU, memory, disk and wall-clock limits per build (VM boundary) and per step (cgroup delegation inside the VM).
4. BuildKit telemetry socket exposure: can it be disabled, and what can a step do with it?
5. Network isolation and worker-host reachability from inside per-build VMs, including the cloud metadata
   endpoint.
6. Cleanup after failed, cancelled and timed-out builds: no leftover VMs, disks, taps, processes or cache state.

Constraints: disposable infrastructure only; synthetic fixtures only; no employer resources, credentials or data;
the instance is deleted at the end and deletion is verified.

### Host options and cost estimate (as of 2026-10-19)

| Option | Instance | Price basis | Estimate for at most 8 hours |
|---|---|---|---|
| A (recommended if you have a personal GCP account) | Google Compute Engine `n2-standard-8` (8 vCPU, 32 GiB, Intel), nested virtualization enabled, 100 GB balanced disk | $0.388/h on-demand (Google pricing page); nested-virtualization support on N2 to be confirmed in Google's docs before provisioning | Compute ≈ $3.11, disk ≈ $0.11, egress ≈ $0.60 (≈5 GB), misc ≈ $0.10. **Total ≈ $4** |
| B (if you have a personal AWS account) | EC2 `m8i.2xlarge` with `NestedVirtualization=enabled` (supported on C8i/M8i/R8i since Feb 2026) | Hourly price not verified in this session; expected to be similar to A | Comparable to A; confirmed from the AWS price list before asking for approval |

Requested approval: **up to $10 total**, with these safeguards:
- the instance is created with an automatic delete after 8 hours (GCP `--max-run-duration` with
  `--instance-termination-action=DELETE`, or an equivalent AWS scheduled termination);
- a billing budget alert at $5 on the account;
- deletion is verified at the end and the final cost reported.

The account must be personal and self-funded. You run any login steps yourself; no credentials are stored in the
repository.
