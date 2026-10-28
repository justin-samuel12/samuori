# Spike: a read-only cgroup view for runtime tests (OD-29)

Date: 2026-10-26. Status: **Blocked inside a rootless BuildKit `RUN` step.** No code changed. Follows the owner's
OD-29 decision (2026-10-26): give the runtime-test container a read-only view of its own cgroup, without host cgroup
write access and nothing beyond that cgroup.

## Method

Source review of the pinned versions only; no VM was available in this session, so nothing was run. Inputs:
BuildKit v0.34.0 (`executor/oci/spec_linux.go`, `executor/oci/spec.go`, `executor/runcexecutor/executor.go`,
`util/rootless/specconv/specconv_linux.go`), containerd v2.4.1 `contrib/seccomp/seccomp_default.go` (the profile
BuildKit v0.34.0 applies), and the run-step diagnostic from the 2026-10-24 practical run
(raw output kept in the maintainer's private records).

## Findings

- **F1 BuildKit removes `/sys` in rootless mode.** BuildKit's spec adds a read-only `cgroup` mount at
  `/sys/fs/cgroup` and the default `sysfs` at `/sys`, but the rootless runc executor then calls `ToRootless`, which
  drops every mount under `/sys` (sysfs cannot be mounted when the daemon's network namespace is not its own, and an
  `rbind,ro` of the host `/sys` would leave `/sys/fs/cgroup` writable) and clears the step's cgroup path and
  resources. The diagnostic matches: no `/sys`, `/proc/self/cgroup` is `0::/`.
- **F2 The step cannot mount anything itself.** Its capabilities are BuildKit's defaults (`CapEff 00000000a80425fb`,
  no `CAP_SYS_ADMIN`). The default seccomp profile, applied to every non-insecure step, allows `mount`, `umount2`,
  `unshare`, `setns` and namespace flags on `clone` only with `CAP_SYS_ADMIN`, and returns `ENOSYS` for `clone3`. So
  the probe can neither mount cgroup2 in place nor create a user, mount and cgroup namespace in which it could (the
  usual unprivileged route).
- **F3 The only BuildKit switch that changes this is `--security=insecure`** (the `security.insecure` entitlement):
  all capabilities, no seccomp, writable cgroupfs and sysfs. That contradicts the owner's constraints and Samuori's
  isolation model and was not considered further.
- **F4 Mounts a Dockerfile step can request** (`bind`, `cache`, `tmpfs`, `secret`, `ssh`) cannot carry a live cgroup
  view; copying cgroup files in would be the synthesized environment OD-29 already rejected.
- **F5 "Its own cgroup" is the daemon's.** Because `ToRootless` clears the step's cgroup path, a rootless step stays in
  the BuildKit daemon's cgroup. With one build per daemon and `cgroup-limits=systemd`, that is the attempt's scope, so
  a correct view would show the attempt's limits.

## Conclusion

The decided behaviour cannot be provided inside a rootless BuildKit `RUN` step without weakening isolation, so it was
not implemented. Paths that keep the security model, each a material change needing owner approval (ADR):

1. Run the runtime test outside BuildKit with the rootless OCI runtime already on the worker (runc, or crun, which is
   GPL and exec-only), under the same RootlessKit: a bundle from the image layout with its own user, mount, network
   (loopback only), PID and cgroup namespaces, a read-only tmpfs at `/sys` and a read-only cgroup2 mount of its own
   cgroup namespace at `/sys/fs/cgroup`, inside the attempt's scope. Needs a self-test proving the view is read-only
   and scoped, and the probe's other constraints kept.
2. Keep the BuildKit step and the documented `BPE_DEFAULT_MEMORY_AVAILABLE=max` workaround for Paketo Node.js images.

`BPE_DEFAULT_MEMORY_AVAILABLE=max` therefore stays documented (`../operators/providers.md`).
